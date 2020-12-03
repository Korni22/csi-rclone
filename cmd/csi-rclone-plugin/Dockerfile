FROM alpine:3.9
RUN apk add --no-cache ca-certificates bash fuse curl unzip tini

RUN curl https://rclone.org/install.sh | bash

COPY ./_output/csi-rclone-plugin /bin/csi-rclone-plugin
RUN chmod 755 /bin/csi-rclone-plugin

ENTRYPOINT ["/sbin/tini", "--", "/bin/csi-rclone-plugin"]

