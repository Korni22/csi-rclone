# Kubernetes CSI rclone mount plugin

![Logo](./images/bucket.png)

<sup><sup>Icons made by <a href="http://www.freepik.com/" title="Freepik">Freepik</a> from <a href="https://www.flaticon.com/" title="Flaticon">www.flaticon.com</a></sup></sup>

This is derivative work by [Jancis](https://github.com/Jancis) which I customized for my needs. First, kudos, this is amazing work!

The major changes are the following:
 * fully configurable by environment variables
 * check mountpoint after rclone forks (rclone forks too fast to be available for the pod)
 * allow definition of remotes on the fly (i.e. for crypt)
 * helm chart [provided](https://diseq.github.io/helm-charts)

Issues:
 * reevaluate the current CSI implementation (i.e. use staging)
 * rclone goes zombie sometimes. not sure if this is a rclone or csi issue.

Usage:

 * deploy helm chart to your cluster
 ```shell
 helm repo add diseq https://diseq.github.io/helm-charts
 helm repo update
 helm install csi-rclone diseq/csi-rclone --create-namespace --namespace csi-rclone
 ```

 * create a PVC and PV
 ```yaml
  apiVersion: v1
  kind: PersistentVolume
  metadata:
    name: pv-demo
    labels:
      name: pv-demo
  spec:
    accessModes:
    - ReadWriteMany
    capacity:
      storage: 10Gi
    storageClassName: rclone
    csi:
      driver: csi-rclone
      volumeHandle: data-id
      volumeAttributes:
        remote: "mydrive"
        remotePath: "/<bucket>/"
        RCLONE_CONFIG_MYDRIVE_TYPE: "s3"
        RCLONE_CONFIG_MYDRIVE_PROVIDER: "other"
        RCLONE_CONFIG_MYDRIVE_ENV_AUTH: "false"
        RCLONE_CONFIG_MYDRIVE_ACCESS_KEY_ID: "<accesskey>"
        RCLONE_CONFIG_MYDRIVE_SECRET_ACCESS_KEY: "<secret>"
        RCLONE_CONFIG_MYDRIVE_ENDPOINT: "https://s3.fr-par.scw.cloud"
        RCLONE_CONFIG_MYDRIVE_LOCATION_CONSTRAINT: "fr-par"
        RCLONE_CONFIG_MYDRIVE_ACL: "private"
        RCLONE_CONFIG_MYDRIVE_REGION: "fr-par"
        RCLONE_CACHE_INFO_AGE: "72h"
        RCLONE_CACHE_CHUNK_CLEAN_INTERVAL: "15m"
        RCLONE_DIR_CACHE_TIME: "5s"
        RCLONE_VFS_CACHE_MODE: "writes"
  ---
  apiVersion: v1
  kind: PersistentVolumeClaim
  metadata:
    name: pv-claim-demo
  spec:
    accessModes:
      - ReadWriteMany
    storageClassName: rclone
    resources:
      requests:
        storage: 10Gi
    volumeName: pv-demo
 ```

 * attach to your pod
 ```yaml
apiVersion: v1
kind: Pod
metadata:
  name: ubuntu
  labels:
    app: ubuntu
spec:
  containers:
  - image: ubuntu
    command:
      - "sleep"
      - "604800"
    imagePullPolicy: IfNotPresent
    name: ubuntu
    volumeMounts:
      - mountPath: "/mnt/data"
        name: data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: pv-claim-demo
  restartPolicy: Always
 ```

 * check mount
 ```shell
 $ kubectl exec -it ubuntu -- bash

root@ubuntu:/# cd /mnt/data/
root@ubuntu:/mnt/data# ls -la
total 686
-rw-r--r-- 1 root root 153481 Nov 29 19:07 dpkg.log
-rw-r--r-- 1 root root 274389 Nov 29 21:06 1.mp3
-rw-r--r-- 1 root root 274389 Nov 29 21:06 2.mp3
root@ubuntu:/mnt/data#
 ```

Sources:
[1](https://github.com/cameronbraid/csi-rclone)
[2](https://github.com/wunderio/csi-rclone)



<snip>

# CSI rclone mount plugin

This project implements Container Storage Interface (CSI) plugin that allows using [rclone mount](https://rclone.org/) as storage backend. Rclone mount points and [parameters](https://rclone.org/commands/rclone_mount/) can be configured using Secret or PersistentVolume volumeAttibutes. 

## Kubernetes cluster compatability
Works:
 - 1.13.x

Does not work: 
 - v1.12.7-gke.10, driver name csi-rclone not found in the list of registered CSI drivers


## Installing CSI driver to kubernetes cluster
TLDR: ` kubectl apply -f deploy/kubernetes --username=admin --password=123`

1. Set up storage backend. You can use [Minio](https://min.io/), Amazon S3 compatible cloud storage service.

2. Configure defaults by pushing secret to kube-system namespace. This is optional if you will always define `volumeAttributes` in PersistentVolume.

```
apiVersion: v1
kind: Secret
metadata:
  name: rclone-secret
type: Opaque
stringData:
  remote: "s3"
  remotePath: "projectname"
  s3-provider: "Minio"
  s3-endpoint: "http://minio-release.default:9000"
  s3-access-key-id: "ACCESS_KEY_ID"
  s3-secret-access-key: "SECRET_ACCESS_KEY"
```

Deploy example secret
> `kubectl apply -f example/kubernetes/rclone-secret-example.yaml --namespace kube-system`

3. You can override configuration via PersistentStorage resource definition. Leave volumeAttributes empty if you don't want to. Keys in `volumeAttributes` will be merged with predefined parameters.

```
apiVersion: v1
kind: PersistentVolume
metadata:
  name: data-rclone-example
  labels:
    name: data-rclone-example
spec:
  accessModes:
  - ReadWriteMany
  capacity:
    storage: 10Gi
  storageClassName: rclone
  csi:
    driver: csi-rclone
    volumeHandle: data-id
    volumeAttributes:
      remote: "s3"
      remotePath: "projectname/pvname"
      s3-provider: "Minio"
      s3-endpoint: "http://minio-release.default:9000"
      s3-access-key-id: "ACCESS_KEY_ID"
      s3-secret-access-key: "SECRET_ACCESS_KEY"
```

Deploy example definition
> `kubectl apply -f example/kubernetes/nginx-example.yaml`


## Building plugin and creating image
Current code is referencing projects repository on github.com. If you fork the repository, you have to change go includes in several places (use search and replace).


1. First push the changed code to remote. The build will use paths from `pkg/` directory.

2. Build the plugin
```
make plugin
```

3. Build the container and inject the plugin into it.
```
make container
```

4. Change docker.io account in `Makefile` and use `make push` to push the image to remote. 
``` 
make push
```
