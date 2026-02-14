# Tailscale CNI testenv

Setup requires a few steps:

* `vagrant up`
* ssh in to each node, `sudo tailscale up --advertise-tags=tag:tailscale-cni-dev`
* `vagrant provision`
* `./deploy.sh`
