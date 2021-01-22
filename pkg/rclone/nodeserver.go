package rclone

import (
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

		//if err := ns.mounter.Unmount(targetPath); err != nil {
		//	klog.Errorf("Unmount directory %s failed with %v", targetPath, err)
		//	return nil, err
		//}

		if err := Unmount(targetPath); err != nil {
			klog.Errorf("Unmount directory %s failed with %v", targetPath, err)
			return nil, err
		}

	}

	mountOptions := req.GetVolumeCapability().GetMount().GetMountFlags()
	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}

	var secret

	// secrets have to be in the csi-rclone namespaces
	if secretName, ok := (req.GetVolumeContext())["secretName"]; ok {
		// Load connection settings from secret
		var e
		secret, e = getSecret(secretName)
	}

	remote, remotePath, mountCommand, flags, e := extractFlags(req.GetVolumeContext(), secret)
	if e != nil {
		klog.Warningf("storage parameter error: %s", e)
		return nil, e
	}

	e = Mount(mountCommand, remote, remotePath, targetPath, flags)
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

func extractFlags(volumeContext map[string]string, secret *v1.Secret) (string, string, string, map[string]string, error) {

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
		return "", "", "", flags, e
	}

	remote := flags["remote"]
	remotePath := flags["remotePath"]
	mountCommand := "mount"

	if remotePathSuffix, ok := flags["remotePathSuffix"]; ok {
		remotePath = remotePath + remotePathSuffix
		delete(flags, "remotePathSuffix")
	}

	if flag, ok := flags["mountCommand"]; ok {
		mountCommand = flag
		delete(flags, "mountCommand")
	}

	delete(flags, "remote")
	delete(flags, "remotePath")
	delete(flags, "secretName")

	return remote, remotePath, mountCommand, flags, nil
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

		//err = util.UnmountPath(req.GetTargetPath(), m)
		//if err != nil {
		//	klog.Error("Error while unmounting path")
		//	return nil, status.Error(codes.Internal, err.Error())
		//}

		if err := Unmount(req.GetTargetPath()); err != nil {
			klog.Errorf("Unmount directory %s failed with %v", targetPath, err)
		} else {
			klog.Infof("Volume %s unmounted successfully", req.VolumeId)
		}
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

func Unmount(targetPath string) error {

	// check if mounted
	args := []string{"-u", "-z", targetPath}
	cmd := exec.Command("/bin/fusermount", args...)

	var waitStatus syscall.WaitStatus
	if err := cmd.Run(); err != nil {
		// Did the command fail because of an unsuccessful exit code
		if exitError, ok := err.(*exec.ExitError); ok {
			waitStatus = exitError.Sys().(syscall.WaitStatus)
			klog.Infof(fmt.Sprintf("%d", waitStatus.ExitStatus()))
			return fmt.Errorf("error unmounting targetpath: %s", targetPath)
		}
		return fmt.Errorf("error unmounting targetpath: %s", targetPath)

	} else {
		// Command was successful
		waitStatus = cmd.ProcessState.Sys().(syscall.WaitStatus)
		klog.Infof(fmt.Sprintf("%d", waitStatus.ExitStatus()))

		klog.Infof("successfully unmounted targetpath=%s", targetPath)
		return nil
	}

}


// func Mount(params mountParams, target string, opts ...string) error {
func Mount(mountCommand string, remote string, remotePath string, targetPath string, flags map[string]string) error {
	mountCmd := "/usr/bin/rclone"
	mountArgs := []string{}

	defaultFlags := map[string]string{}
	defaultFlags["allow-other"] = "true"

	// rclone mount remote:path /path/to/mountpoint [flags]

    // mount runs in foreground and started as process
	mountArgs = append(
		mountArgs,
        // required for exec, argv[0]
		//mountCmd,
		//"mount",
		mountCommand,
		fmt.Sprintf("%s:%s", remote, remotePath),
		targetPath,
		"--daemon",
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

	klog.Infof("executing mount command cmd=%s, remote=%s:%s, options=%v, targetpath=%s", mountCmd, remote, remotePath, mountArgs, targetPath)

	cmd := exec.Command(mountCmd, mountArgs...)
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mounting failed: %v cmd: '%s' remote: '%s:%s' targetpath: %s output: %q",
			err, mountCmd, remote, remotePath, targetPath, string(out))
	}


	//err = syscall.Exec(mountCmd, mountArgs, env)

    //procAttr := syscall.ProcAttr{
    //    Env:   env,
    //    Sys: &syscall.SysProcAttr{
    //        Foreground: false,
    //    },
	//}
	//_, err = syscall.ForkExec(mountCmd, mountArgs, &procAttr)

	//if err != nil {
	//	return fmt.Errorf("mounting failed: %v cmd: '%s' remote: '%s:%s' targetpath: %s output: %q",
	//		err, mountCmd, remote, remotePath, targetPath)
	//}

	////var sysproc = &syscall.SysProcAttr{ Foreground:false, Noctty:true }
	////var cred =  &syscall.Credential{ os.Getuid(), os.Getgid(), []uint32{} }
	//var sysproc = &syscall.SysProcAttr {
	//	/*Chroot:     "",
	//	Credential: nil,
	//	//Ptrace:     true,
	//	Setsid:     false,
	//	Setpgid:    false,
	//	Setctty:    false,
	//	Noctty:     false,
	//	Ctty:       0,
	//	Pdeathsig:  syscall.SIGCHLD,*/
	//	Foreground: false,
	//	Setsid:     true,
	//}

    //var procAttr = os.ProcAttr{
	//	Dir: "/",
	//	Env: env,
	//	Files: []*os.File{
	//		os.Stdin,
	//		os.Stdout,
	//		os.Stderr,
	//    },
    //    Sys: sysproc,
    //}

	//process, err := os.StartProcess(mountCmd, mountArgs, &procAttr)

	//if err == nil {
    //    err = process.Release()
    //    if err != nil {
	//		return fmt.Errorf("mounting failed - error releasing process: %v cmd: '%s' remote: '%s:%s' targetpath: %s output: %q",
	//		err, mountCmd, remote, remotePath, targetPath)
	//	}
	//} else {
	//	return fmt.Errorf("mounting failed - error mounting: %v cmd: '%s' remote: '%s:%s' targetpath: %s output: %q",
	//		err, mountCmd, remote, remotePath, targetPath)
	//}


	iterations := 0
	for {
	    klog.Infof("waiting for mountpoint targetpath=%s", targetPath)
	    time.Sleep(1000 * time.Millisecond)
		iterations = iterations + 1


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
