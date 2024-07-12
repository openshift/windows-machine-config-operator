#!/bin/bash

if [ "$#" -lt 1 ]; then
	echo "Error: incorrect parameter count $#" >&2
	exit 1
fi

# Pass in source file with govc creds/settings e.g. sourcedevqe
source $1

# Get all or a specific host
govc ls /*/vm/windows-golden-images/windows-server-2022-template-ipv6-disabled > servers.txt
#govc ls /*/vm/windows-golden-images/ > servers.txt

# Get the list of servers from a file
servers=$(cat servers.txt)

# Ping each server and print the results
for server in $servers; do
	govc export.ovf -vm $server .
done
