#!/bin/bash

# Run a packet trace on Windows nodes in an OpenShift cluster.
# Requires SSH bastion deployed on cluster - https://github.com/eparis/ssh-bastion

function help() {
    echo "Usage: packet_trace.sh [OPTIONS]... start/stop node_name"
    echo "Run a packet trace on Windows nodes in an OpenShift cluster"
    echo "Requires SSH bastion deployed on cluster - https://github.com/eparis/ssh-bastion"
    echo ""
    echo "Commands:"
    echo "start    Starts a packet trace on specified node"
    echo "stop     Stops a packet trace on specified node. Copies the trace as a both a .pcapng and a .etl into the current directory."
    echo ""
    echo "Options:"
    echo "-d=   Directory to save packet traces, used in stop command"
    echo "-u=   Windows username, defaults to Administrator"
    echo "-i=   Path to SSH key used to configure Windows nodes"
    echo ""
    echo "Examples:"
    echo "packet_trace.sh -u capi start win-node-1"
    echo "packet_trace.sh -u capi -d /traces stop win-node-1"
    echo "packet_trace.sh -i ~/.ssh/key.pem start win-node-2"
}

function winssh() {
    local ssh_command=$1
    local node=$2
    ingress_host="$(oc get service --all-namespaces -l run=ssh-bastion -o go-template='{{ with (index (index .items 0).status.loadBalancer.ingress 0) }}{{ or .hostname .ip }}{{end}}')"
    ssh $SSH_KEY_PARAM -t -o StrictHostKeyChecking=no -o ProxyCommand="ssh $SSH_KEY_PARAM -A -o StrictHostKeyChecking=no -o ServerAliveInterval=30 -W %h:%p core@${ingress_host}" $USERNAME@$node \""$ssh_command"\"
}

function winscp() {
    local srcpath=$1
    local dstpath=$2
    local node=$3
    ingress_host="$(oc get service --all-namespaces -l run=ssh-bastion -o go-template='{{ with (index (index .items 0).status.loadBalancer.ingress 0) }}{{ or .hostname .ip }}{{end}}')"
    scp -o ProxyCommand="ssh -A -o StrictHostKeyChecking=no -o ServerAliveInterval=30 -W %h:%p core@${ingress_host}" $USERNAME@$node:$srcpath $dstpath

}

function start() {
    local node=$1
    SSH_COMMAND="powershell; wget https://raw.githubusercontent.com/microsoft/SDN/master/Kubernetes/windows/debug/collectlogs.ps1 -o collectlogs.ps1; ./collectlogs.ps1; cd c:\\k\\debug; ./startpacketcapture.cmd"
    winssh "$SSH_COMMAND" "$node"
}

function stop() {
    local node=$1
    SSH_COMMAND="powershell; cd c:\\k\\debug; ./stoppacketcapture.cmd; wget https://github.com/microsoft/etl2pcapng/releases/download/v1.4.0/etl2pcapng.zip -o etl2pcapng.zip; Expand-Archive -Force etl2pcapng.zip; ./etl2pcapng/etl2pcapng/x64/etl2pcapng.exe /server.etl trace.pcapng"
    winssh "$SSH_COMMAND" "$node"
}

function gather() {
    local node=$1
    winscp "/server.etl" $DIR/server.etl $node
    winscp "/k/debug/trace.pcapng" $DIR/trace.pcapng $node
}

DIR="."
USERNAME="Administrator"
SSH_KEY_PARAM=""
while getopts ":d:u:i:h" opt; do
    case "$opt" in
    d) DIR=$OPTARG;;
    u) USERNAME=$OPTARG;;
    i) SSH_KEY_PARAM="-i $OPTARG";;
    h) help; exit 0;;
    ?) help; exit 1;;
    esac
done
shift $((OPTIND -1))

COMMAND=$1
NODE=$2
if [ "$COMMAND" != "start" ] && [ "$COMMAND" != "stop" ]; then
    help
    exit 1
fi

if [ -z "$NODE" ]; then
    echo $COMMAND requires a node name to be provided
    exit 1
fi

if [ "$COMMAND" == "start" ];then
    start $NODE || exit 1
    exit 0
fi;
if [ "$COMMAND" == "stop" ];then
    stop $NODE || exit 1
    gather $NODE || exit 1
    exit 0
fi;
