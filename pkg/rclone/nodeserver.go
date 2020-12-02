package rclone

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"time"
	"syscall"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/volume/util"

	csicommon "github.com/kubernetes-csi/drivers/pkg/csi-common"
)

type nodeServer struct {
	*csicommon.DefaultNodeServer
	mounter *mount.SafeFormatAndMount
}

type mountPoint struct {
	VolumeId  string
	MountPath string
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.Infof("NodePublishVolume: called with args %+v", *req)

	targetPath := req.GetTargetPath()

	notMnt, err := mount.New("").IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(targetPath, 0750); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			notMnt = true
		} else {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if !notMnt {
		// testing original mount point, make sure the mount link is valid
		if _, err := ioutil.ReadDir(targetPath); err == nil {
			klog.Infof("already mounted to target %s", targetPath)
			return &csi.NodePublishVolumeResponse{}, nil
		}
		// todo: mount link is invalid, now unmount and remount later (built-in functionality)
		klog.Warningf("ReadDir %s failed with %v, unmount this directory", targetPath, err)

		ns.mounter = &mount.SafeFormatAndMount{
			Interface: mount.New(""),
			Exec:      mount.NewOsExec(),
		}

		if err := ns.mounter.Unmount(targetPath); err != nil {
			klog.Errorf("Unmount directory %s failed with %v", targetPath, err)
			return nil, err
		}
	}

	mountOptions := req.GetVolumeCapability().GetMount().GetMountFlags()
	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}

	// Load default connection settings from secret
	secret, e := getSecret("rclone-secret")

	remote, remotePath, flags, e := extractFlags(req.GetVolumeContext(), secret)
	if e != nil {
		klog.Warningf("storage parameter error: %s", e)
		return nil, e
	}

	e = Mount(remote, remotePath, targetPath, flags)
	if e != nil {
		if os.IsPermission(e) {
			return nil, status.Error(codes.PermissionDenied, e.Error())
		}
		if strings.Contains(e.Error(), "invalid argument") {
			return nil, status.Error(codes.InvalidArgument, e.Error())
		}
		return nil, status.Error(codes.Internal, e.Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func extractFlags(volumeContext map[string]string, secret *v1.Secret) (string, string, map[string]string, error) {

	// Empty argument list
	flags := make(map[string]string)

	// Secret values are default, gets merged and overriden by corresponding PV values
	if secret != nil && secret.Data != nil && len(secret.Data) > 0 {

		// Needs byte to string casting for map values
		for k, v := range secret.Data {
			flags[k] = string(v)
		}
	} else {
		klog.Infof("No csi-rclone connection defaults secret found.")
	}

	if len(volumeContext) > 0 {
		for k, v := range volumeContext {
			flags[k] = v
		}
	}

	if e := validateFlags(flags); e != nil {
		return "", "", flags, e
	}

	remote := flags["remote"]
	remotePath := flags["remotePath"]

	if remotePathSuffix, ok := flags["remotePathSuffix"]; ok {
		remotePath = remotePath + remotePathSuffix
		delete(flags, "remotePathSuffix")
	}

	delete(flags, "remote")
	delete(flags, "remotePath")

	return remote, remotePath, flags, nil
}

func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {

	klog.Infof("NodeUnPublishVolume: called with args %+v", *req)

	targetPath := req.GetTargetPath()
	if len(targetPath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "NodeUnpublishVolume Target Path must be provided")
	}

	m := mount.New("")

	notMnt, err := m.IsLikelyNotMountPoint(targetPath)
	if err != nil && !mount.IsCorruptedMnt(err) {
		return nil, status.Error(codes.Internal, err.Error())
	}

	if notMnt && !mount.IsCorruptedMnt(err) {
		klog.Infof("Volume not mounted")

	} else {
		err = util.UnmountPath(req.GetTargetPath(), m)
		if err != nil {
			klog.Error("Error while unmounting path")
			return nil, status.Error(codes.Internal, err.Error())
		}

		klog.Infof("Volume %s unmounted successfully", req.VolumeId)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	klog.Infof("NodeUnstageVolume: called with args %+v", *req)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	klog.Infof("NodeStageVolume: called with args %+v", *req)
	return &csi.NodeStageVolumeResponse{}, nil
}

func validateFlags(flags map[string]string) error {
	if _, ok := flags["remote"]; !ok {
		return status.Errorf(codes.InvalidArgument, "missing volume context value: remote")
	}
	if _, ok := flags["remotePath"]; !ok {
		return status.Errorf(codes.InvalidArgument, "missing volume context value: remotePath")
	}
	return nil
}

func getSecret(secretName string) (*v1.Secret, error) {
	clientset, e := GetK8sClient()
	if e != nil {
		return nil, status.Errorf(codes.Internal, "can not create kubernetes client: %s", e)
	}

	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)

	namespace, _, err := kubeconfig.Namespace()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "can't get current namespace, error %s", secretName, err)
	}

	klog.Infof("Loading csi-rclone connection defaults from secret %s/%s", namespace, secretName)

	secret, e := clientset.CoreV1().
		Secrets(namespace).
		Get(secretName, metav1.GetOptions{})

	if e != nil {
		return nil, status.Errorf(codes.Internal, "can't load csi-rclone settings from secret %s: %s", secretName, e)
	}

	return secret, nil
}

func flagToEnvName(flag string) string {
	// To find the name of the environment variable, first, take the long option name, strip the leading --, change - to _, make upper case and prepend RCLONE_.
	flag = strings.ToUpper(flag)
	flag = strings.ReplaceAll(flag, "-", "_")
	return fmt.Sprintf("RCLONE_%s", flag)
}

func userFlagToEnvName(flag string) string {
	// To find the name of the environment variable, first, take the long option name, strip the leading --, change - to _, make upper case and prepend RCLONE_.
	flag = strings.ToUpper(flag)
	flag = strings.ReplaceAll(flag, "-", "_")
	return flag
}

// func Mount(params mountParams, target string, opts ...string) error {
func Mount(remote string, remotePath string, targetPath string, flags map[string]string) error {
	mountCmd := "rclone"
	mountArgs := []string{}

	defaultFlags := map[string]string{}
	defaultFlags["allow-other"] = "true"
	defaultFlags["allow-root"] = "true"

	// rclone mount remote:path /path/to/mountpoint [flags]

    // mount not allowed to block
	mountArgs = append(
		mountArgs,
		"mount",
		fmt.Sprintf("%s:%s", remote, remotePath),
		targetPath
	)

    env := os.Environ()

	// Add default flags
	for k, v := range defaultFlags {
		// Exclude overriden flags
		if _, ok := flags[k]; !ok {
			env = append(env, fmt.Sprintf("%s=%s", flagToEnvName(k), v))
		}
	}

	// Add user supplied flags
	for k, v := range flags {
		env = append(env, fmt.Sprintf("%s=%s", userFlagToEnvName(k), v))
	}

	// create target, os.Mkdirall is noop if it exists
	err := os.MkdirAll(targetPath, 0750)
	if err != nil {
		return err
	}

	klog.Infof("executing mount command cmd=%s, remote=:%s:%s, targetpath=%s", mountCmd, remote, remotePath, targetPath)

	cmd := exec.Command(mountCmd, mountArgs...)
	cmd.Env = env

	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b

	err = cmd.Start()
	pid := cmd.Process.Pid
	if err != nil {
		return fmt.Errorf("mounting failed: %v cmd: '%s' remote: '%s:%s' targetpath: %s",
			err, mountCmd, remote, remotePath, targetPath)
	}

	iterations := 0
	for {
	        klog.Infof("waiting for mountpoint targetpath=%s", targetPath)
	        time.Sleep(1000 * time.Millisecond)
		iterations = iterations + 1

		// check if process is alive
		process, err := os.FindProcess(int(pid))
		if err != nil {
			fmt.Printf("Failed to find process: %s\n", err)
			return fmt.Errorf("mounting failed, process not found: %v cmd: '%s' remote: '%s:%s' targetpath: %s", err, mountCmd, remote, remotePath, targetPath)
		} else {
			err := process.Signal(syscall.Signal(0))
			fmt.Printf("process.Signal on pid %d returned: %v\n", pid, err)

			if(!(err == nil)) {
				return fmt.Errorf("mounting failed, process died: %v cmd: '%s' remote: '%s:%s' targetpath: %s output: %q", err, mountCmd, remote, remotePath, targetPath, string(b.Bytes()))
			}
		}

	        // check if mounted
	        args := []string{"-q", targetPath}
		cmd := exec.Command("/bin/mountpoint", args...)

		var waitStatus syscall.WaitStatus
		if err := cmd.Run(); err != nil {
			// Did the command fail because of an unsuccessful exit code
			if exitError, ok := err.(*exec.ExitError); ok {
				waitStatus = exitError.Sys().(syscall.WaitStatus)
				klog.Infof(fmt.Sprintf("%d", waitStatus.ExitStatus()))			
			}
		} else {
		    // Command was successful
		    waitStatus = cmd.ProcessState.Sys().(syscall.WaitStatus)
		    klog.Infof(fmt.Sprintf("%d", waitStatus.ExitStatus()))

			klog.Infof("mountpoint ready targetpath=%s", targetPath)
			return nil
		}

		if( iterations == 30) {
			klog.Infof("mounting timed out targetpath=%s", targetPath)
			return fmt.Errorf("mounting timed-out: %v cmd: '%s' remote: '%s:%s' targetpath: %s", err, mountCmd, remote, remotePath, targetPath)
		}

	}

	return nil
}
