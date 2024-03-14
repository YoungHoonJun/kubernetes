FROM busybox
ADD ./_output/local/bin/linux/amd64/kube-scheduler /usr/local/bin/kube-scheduler
ADD ./huni_scheduler.conf /etc/kubernetes/scheduler.conf
ADD ./huni_config.yaml /etc/kubernetes/my-scheduler/my-scheduler-config.yaml
