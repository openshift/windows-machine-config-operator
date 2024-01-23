# Copying VMs to CI vCenters

Tools to copy an existing VM images from one vCenter environment to another.
Keep the images to transfer as VMs (powered-off state), not as templates.

Note that this should only be done as a last resort.
The ideal way is to use packer to build a golden image replica within each separate environment.
If building via packer is not working, we should first try to fix the packer scripts before trying to copy over VMs.

## Installing govc

Install `govc` on the host where you will be building image. Download, unzip, and move the utility within your `$PATH`.
e.g.

```
curl -L -o - "https://github.com/vmware/govmomi/releases/latest/download/govc_$(uname -s)_$(uname -m).tar.gz" | tar -C /usr/local/bin -xvzf - govc
```

## Prerequisite files

Please ensure the following file are present in the location where you are running the cloning scripts from:

    - sourcedevqe
    - sourceibm-ci
    - sourcev8c-2
    - sourcevcs8e
    - options.json

Fill out the config variables with your credentials for each environment, 1 DEV/QE and 3 CI.

## Scripts

There are two scripts to run, each of which requires passing in a source file to populate govc config variables.

    - getem.sh
    - putem.sh

You can run these from a "jump" VM in vSphere or from your local machine, but the latter will be slower in the transfer.
