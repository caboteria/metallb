# MetalLB Minimal Controller

MetalLB is a load-balancer implementation for bare
metal [Kubernetes](https://kubernetes.io) clusters, using standard
routing protocols.

This is an experiment to see what the absolute minimal controller
would look like.  It requires no configuration, but does require that
the --load-balancer-ip be provided for all load balancers.  All this
controller does is copy that IP from the service configuration to the
service status so the speakers can advertise the service.

This runs instead of the metallb controller, and it's probably easiest
to run it outside of kubernetes.

```
$ ./build/amd64/controller/controller -kubeconfig $PATH_TO_KUBECONFIG_FILE
```
