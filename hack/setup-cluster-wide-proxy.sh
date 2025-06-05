#!/bin/bash
#
# setup-cluster-wide-proxy.sh - create the required resources and configures a cluster-wide proxy for an Openshift cluster
# 
#
# USAGE
#    setup-cluster-wide-proxy.sh
#
# ENVIRONMENT
#    SSH_PUB_KEY_FILE               path to the public key used for proxy ignition, default to $HOME/.ssh/openshift-dev.pub
#    AWS_SHARED_CREDENTIALS_FILE    path to the AWS credential file, default to $HOME/.aws/credentials
#
# PREREQUISITES
#    oc                   to fetch cluster info on the cluster (should be logged in)
#    aws                  to fetch AWS platform specific information (should be configured)
#    yq                   to process yaml content
#    jq                   to process json content
#    curl                 to fetch resources over the network#

set -o nounset
set -o errexit
set -o pipefail

trap 'CHILDREN=$(jobs -p); if test -n "${CHILDREN}"; then kill ${CHILDREN} && wait; fi' TERM

function generate_proxy_ignition() {
  ssh_pub_key=$(cat $SSH_PUB_KEY_FILE)
  cat > ${TEMP_DIR}/${PROXY_IGNITION_FILENAME} << EOF
{
  "ignition": {
    "config": {},
    "security": {
      "tls": {}
    },
    "timeouts": {},
    "version": "3.0.0"
  },
  "passwd": {
    "users": [
      {
        "name": "core",
        "sshAuthorizedKeys": [
          "${ssh_pub_key}"
        ]
      }
    ]
  },
  "storage": {
    "files": [
      {
        "path": "/etc/squid/passwords",
        "contents": {
          "source": "data:text/plain;base64,${HTPASSWD_CONTENTS}"
        },
        "mode": 420
      },
      {
        "path": "/etc/squid/squid.conf",
        "contents": {
          "source": "data:text/plain;base64,${SQUID_CONFIG}"
        },
        "mode": 420
      },
      {
        "path": "/etc/squid.sh",
        "contents": {
          "source": "data:text/plain;base64,${SQUID_SH}"
        },
        "mode": 420
      },
      {
        "path": "/etc/squid/proxy.sh",
        "contents": {
          "source": "data:text/plain;base64,${PROXY_SH}"
        },
        "mode": 420
      }
    ]
  },
  "systemd": {
    "units": [
      {
        "contents": "[Unit]\nWants=network-online.target\nAfter=network-online.target\n[Service]\n\nStandardOutput=journal+console\nExecStart=bash /etc/squid.sh\n\n[Install]\nRequiredBy=multi-user.target\n",
        "enabled": true,
        "name": "squid.service"
      },
      {
        "dropins": [
          {
            "contents": "[Service]\nExecStart=\nExecStart=/usr/lib/systemd/systemd-journal-gatewayd \\\n  --key=/opt/openshift/tls/journal-gatewayd.key \\\n  --cert=/opt/openshift/tls/journal-gatewayd.crt \\\n  --trust=/opt/openshift/tls/root-ca.crt\n",
            "name": "certs.conf"
          }
        ],
        "name": "systemd-journal-gatewayd.service"
      }
    ]
  }
}
EOF
  echo "Generated proxy ignition in: ${TEMP_DIR}/${PROXY_IGNITION_FILENAME}"
}

function generate_proxy_aws_template() {
  cat > ${TEMP_DIR}/cluster_proxy_aws_template.yaml << EOF
AWSTemplateFormatVersion: 2010-09-09
Description: Template for OpenShift Cluster Proxy (EC2 Instance, Security Groups and IAM)
Parameters:
  InfrastructureName:
    AllowedPattern: ^([a-zA-Z][a-zA-Z0-9\-]{0,26})$
    MaxLength: 27
    MinLength: 1
    ConstraintDescription: Infrastructure name must be alphanumeric, start with a letter, and have a maximum of 27 characters.
    Description: A short, unique cluster ID used to tag cloud resources and identify items owned or used by the cluster.
    Type: String
  Ami:
    Description: Current CoreOS AMI to use for proxy.
    Type: AWS::EC2::Image::Id
  AllowedProxyCidr:
    AllowedPattern: ^(([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])\.){3}([0-9]|[1-9][0-9]|1[0-9]{2}|2[0-4][0-9]|25[0-5])(\/([0-9]|1[0-9]|2[0-9]|3[0-2]))$
    ConstraintDescription: CIDR block parameter must be in the form x.x.x.x/0-32.
    Default: 0.0.0.0/0
    Description: CIDR block to allow access to the proxy node.
    Type: String
  ClusterName:
    Description: The cluster name used to uniquely identify the proxy load balancer
    Type: String
  PublicSubnet:
    Description: The public subnet to launch the proxy node into.
    Type: AWS::EC2::Subnet::Id
  VpcId:
    Description: The VPC-scoped resources will belong to this VPC.
    Type: AWS::EC2::VPC::Id
  ProxyIgnitionLocation:
    Default: s3://my-s3-bucket/proxy.ign
    Description: Ignition config file location.
    Type: String
Metadata:
  AWS::CloudFormation::Interface:
    ParameterGroups:
    - Label:
        default: "Cluster Information"
      Parameters:
      - InfrastructureName
    - Label:
        default: "Host Information"
      Parameters:
      - Ami
      - ProxyIgnitionLocation
    - Label:
        default: "Network Configuration"
      Parameters:
      - VpcId
      - AllowedProxyCidr
      - PublicSubnet
      - ClusterName
    ParameterLabels:
      InfrastructureName:
        default: "Infrastructure Name"
      VpcId:
        default: "VPC ID"
      AllowedProxyCidr:
        default: "Allowed ingress Source"
      Ami:
        default: "CoreOS AMI ID"
      ProxyIgnitionLocation:
        default: "Bootstrap Ignition Source"
      ClusterName:
        default: "Cluster name"
Resources:
  ProxyIamRole:
    Type: AWS::IAM::Role
    Properties:
      AssumeRolePolicyDocument:
        Version: "2012-10-17"
        Statement:
        - Effect: "Allow"
          Principal:
            Service:
            - "ec2.amazonaws.com"
          Action:
          - "sts:AssumeRole"
      Path: "/"
      Policies:
      - PolicyName: !Join ["-", [!Ref InfrastructureName, "proxy", "policy"]]
        PolicyDocument:
          Version: "2012-10-17"
          Statement:
          - Effect: "Allow"
            Action: "ec2:Describe*"
            Resource: "*"
  ProxyInstanceProfile:
    Type: "AWS::IAM::InstanceProfile"
    Properties:
      Path: "/"
      Roles:
      - Ref: "ProxyIamRole"
  ProxySecurityGroup:
    Type: AWS::EC2::SecurityGroup
    Properties:
      GroupDescription: Cluster Proxy Security Group
      SecurityGroupIngress:
      - IpProtocol: tcp
        FromPort: 22
        ToPort: 22
        CidrIp: 0.0.0.0/0
      - IpProtocol: tcp
        ToPort: 3128
        FromPort: 3128
        CidrIp: !Ref AllowedProxyCidr
      - IpProtocol: tcp
        ToPort: 19531
        FromPort: 19531
        CidrIp: !Ref AllowedProxyCidr
      VpcId: !Ref VpcId
  ProxyInstance:
    Type: AWS::EC2::Instance
    Properties:
      ImageId: !Ref Ami
      IamInstanceProfile: !Ref ProxyInstanceProfile
      KeyName: "openshift-dev"
      InstanceType: "m5.large"
      NetworkInterfaces:
      - AssociatePublicIpAddress: "true"
        DeviceIndex: "0"
        GroupSet:
        - !Ref "ProxySecurityGroup"
        SubnetId: !Ref "PublicSubnet"
      UserData:
        Fn::Base64: !Sub
        - '{"ignition":{"config":{"replace":{"source":"\${IgnitionLocation}"}},"version":"3.0.0"}}'
        - {
          IgnitionLocation: !Ref ProxyIgnitionLocation
        }
Outputs:
  ProxyId:
    Description: The proxy node instanceId.
    Value: !Ref ProxyInstance
  ProxyPrivateIp:
    Description: The proxy node private IP address.
    Value: !GetAtt ProxyInstance.PrivateIp
  ProxyPublicIp:
    Description: The proxy node public IP address.
    Value: !GetAtt ProxyInstance.PublicIp
EOF
  echo "Generated cluster proxy AWS cloudformation template in: ${TEMP_DIR}/cluster_proxy_aws_template.yaml"
}

function join_by { 
  local IFS="$1"; shift; echo "$*"; 
}

# fetch infra information
INFRA_CLUSTER_YAML=$(oc get infrastructure cluster -o yaml)
INFRASTRUCTURE_NAME=$(echo "${INFRA_CLUSTER_YAML}" | yq e '.status.infrastructureName' -)
PLATFORM=$(echo "${INFRA_CLUSTER_YAML}" | yq e '.status.platform' -)

# check supported platforms, just AWS as of now.
case "$PLATFORM" in
    AWS)
      echo "Initializing cluster-wide proxy for cluster ${INFRASTRUCTURE_NAME} on ${PLATFORM}"
    ;;
    *)
      error-exit "Platform '$platform' is not yet supported by this script"
    ;;
esac


# check required environment variables and load defaults
SSH_PUB_KEY_FILE=${SSH_PUB_KEY_FILE:-}
if [ -z "${SSH_PUB_KEY_FILE}" ]; then
  SSH_PUB_KEY_FILE="$HOME/.ssh/openshift-dev.pub"
  echo "SSH_PUB_KEY_FILE not set, using default ${SSH_PUB_KEY_FILE}"
fi

AWS_SHARED_CREDENTIALS_FILE=${AWS_SHARED_CREDENTIALS_FILE:-}
if [ -z "${AWS_SHARED_CREDENTIALS_FILE}" ]; then
  AWS_SHARED_CREDENTIALS_FILE="$HOME/.aws/credentials"
  echo "AWS_SHARED_CREDENTIALS_FILE not set, using default ${AWS_SHARED_CREDENTIALS_FILE}"
fi

# process region and cluster name
REGION=$(echo "${INFRA_CLUSTER_YAML}" | yq e '.status.platformStatus.aws.region' -)
test -n "${REGION}"

PROXY_NAME="${INFRASTRUCTURE_NAME}-proxy"
echo "Setting up cluster-wide proxy with name ${PROXY_NAME} for ${PLATFORM} in region ${REGION}"

# process coreOS AMIs
COREOS_IMAGE_STREAM_URL="https://builds.coreos.fedoraproject.org/streams/stable.json"
COREOS_IMAGE_STREAMS=$(curl -sL "${COREOS_IMAGE_STREAM_URL}")
AMI=$(echo "${COREOS_IMAGE_STREAMS}" | jq -r .architectures.x86_64.images.aws.regions[\"${REGION}\"].image)
if [ -z "${AMI}" ]; then
  echo "Cannot find CoreOS AMI for region ${REGION}. See ${COREOS_IMAGE_STREAM_URL}" 1>&2
  exit 1
fi
RELEASE=$(echo "${COREOS_IMAGE_STREAMS}" | jq -r .architectures.x86_64.images.aws.regions[\"${REGION}\"].release)
if [ -z "${RELEASE}" ]; then
  echo "Cannot find CoreOS AMI release for region ${REGION}. See ${COREOS_IMAGE_STREAM_URL}" 1>&2
  exit 1
fi
echo "Found CoreOS image with id ${AMI} and release ${RELEASE} for ${PLATFORM} in region ${REGION}"

# user worker node information to obtain VPC ID from the subnet
WORKER_PROVIDER_ID=$(oc get nodes -l node-role.kubernetes.io/worker -o jsonpath='{.items[0].spec.providerID}')
WORKER_INSTANCE_ID=$(echo "${WORKER_PROVIDER_ID}" | cut -d '/' -f5)
WORKER_SUBNET_ID=$(aws ec2 describe-instances --instance-ids "${WORKER_INSTANCE_ID}" --query 'Reservations[].Instances[].SubnetId' --region "${REGION}" --output text)
if [[ -z "$WORKER_SUBNET_ID" ]]; then
  echo "Cannot find a subnet for worker node with provider ${WORKER_PROVIDER_ID} in region ${REGION}"
  exit 1
fi
echo "Found subnet ${WORKER_SUBNET_ID} for worker node with instance id ${WORKER_INSTANCE_ID} in region ${REGION}"

# pick the VPC id for the first subnet
VPC_ID=$(aws ec2 describe-subnets --subnet-ids "${WORKER_SUBNET_ID}" --region "${REGION}" --output json | jq -r '.[][0].VpcId')
echo "Found VPC with id ${VPC_ID} for subnet ${WORKER_SUBNET_ID} in region ${REGION}"

# fetch all the subnets ids for the given VPC
echo "Finding all subnets associated with ${VPC_ID}"
SUBNETS_IDS=$(aws ec2 describe-subnets --filters "Name=vpc-id,Values=${VPC_ID}" --region "${REGION}" --output json | jq -r '.Subnets[].SubnetId')

# loop the subnets and pick the first public subnet, i.e. a subnet with an associated gateway in the route tables
declare PUBLIC_SUBNET_ID
for subnetId in ${SUBNETS_IDS[@]}; do
  echo "Finding route tables with public gateway associated with subnet ${subnetId}"
  #gatewayId=$(aws ec2 describe-route-tables --filters Name=association.subnet-id,Values="${subnetId}" --region "${REGION}" --output json | grep '"GatewayId": "igw.*'  1>&2 > /dev/null)
  if aws ec2 describe-route-tables --filters Name=association.subnet-id,Values="${subnetId}" --region "${REGION}" --output json | grep '"GatewayId": "igw.*'  1>&2 > /dev/null; then
    PUBLIC_SUBNET_ID="${subnetId}"
    echo "  Found public subnet ${subnetId} with available gateway"
    break
  else    
    echo "  Skipping subnet ${subnetId}, no gateway found in route tables"
  fi
done

# ensure public subnet exist
if [[ -z "$PUBLIC_SUBNET_ID" ]]; then
  echo "Cannot find a public subnet in for VPC with id ${VPC_ID}"
  exit 1
fi
echo ""

# generate proxy creds
PASSWORD="$(uuidgen | sha256sum | cut -b -32)"
HTPASSWD_CONTENTS="${PROXY_NAME}:$(openssl passwd -apr1 ${PASSWORD})"
echo "Generated PROXY PASSWORD: ${PASSWORD}"
echo "Generated HTPASSWD_CONTENTS: ${HTPASSWD_CONTENTS}"
HTPASSWD_CONTENTS="$(echo -e ${HTPASSWD_CONTENTS} | base64 -w0)"
echo ""

# define squid config
SQUID_CONFIG="$(base64 -w0 << EOF
http_port 3128
cache deny all
access_log stdio:/tmp/squid-access.log all
debug_options ALL,1
shutdown_lifetime 0
auth_param basic program /usr/lib64/squid/basic_ncsa_auth /squid/passwords
auth_param basic realm proxy
acl authenticated proxy_auth REQUIRED
http_access allow all
pid_filename /tmp/proxy-setup
EOF
)"
# http_access allow authenticated

# define squid.sh
export PROXY_IMAGE="registry.ci.openshift.org/origin/4.5:egress-http-proxy"
SQUID_SH="$(base64 -w0 << EOF
#!/bin/bash
podman run --entrypoint='["bash", "/squid/proxy.sh"]' --expose=3128 --net host --volume /etc/squid:/squid:Z ${PROXY_IMAGE}
EOF
)"

# define proxy.sh
PROXY_SH="$(base64 -w0 << EOF
#!/bin/bash
function print_logs() {
    while [[ ! -f /tmp/squid-access.log ]]; do
    sleep 5
    done
    tail -f /tmp/squid-access.log
}
print_logs &
squid -N -f /squid/squid.conf
EOF
)"

# create temp dir for ignition
TEMP_DIR=`mktemp -d`

PROXY_IGNITION_FILENAME="proxy.ign"
generate_proxy_ignition

# create the s3 bucket to push to
aws s3 ls s3://${PROXY_NAME} || aws --region "${REGION}" s3 mb "s3://${PROXY_NAME}"
echo "listed or created S3 bucket ${PROXY_NAME} in region ${REGION}"

aws --region "${REGION}" s3api put-public-access-block \
  --bucket "${PROXY_NAME}"\
  --public-access-block-configuration "BlockPublicAcls=false,IgnorePublicAcls=false,BlockPublicPolicy=false,RestrictPublicBuckets=false"
echo "Updating S3 bucket ${PROXY_NAME}: put-public-access-block succeeded"

aws s3api put-bucket-ownership-controls --bucket "${PROXY_NAME}" --ownership-controls="Rules=[{ObjectOwnership=ObjectWriter}]"
echo "Updating S3 bucket ${PROXY_NAME}: put-bucket-ownership-controls succeeded"

aws --region "${REGION}" s3api put-bucket-acl --bucket "${PROXY_NAME}" --acl public-read
echo "Updating S3 bucket ${PROXY_NAME}: put-bucket-acl succeeded"

# create ignition entries for certs and script to start squid and systemd unit entry
# create the proxy stack and then get its IP
PROXY_IGNITION_LOCATION_S3_URI="s3://${PROXY_NAME}/${PROXY_IGNITION_FILENAME}"

# push the generated ignition to the s3 bucket
aws --region "${REGION}" s3 cp ${TEMP_DIR}/${PROXY_IGNITION_FILENAME} "${PROXY_IGNITION_LOCATION_S3_URI}"
echo "Uploaded ${TEMP_DIR}/${PROXY_IGNITION_FILENAME} to S3 bucket ${PROXY_NAME} in region ${REGION}"

aws --region "${REGION}" s3api put-object-acl --bucket "${PROXY_NAME}" --key "${PROXY_IGNITION_FILENAME}" --acl public-read #--acl bucket-owner-full-control
echo "Updating S3 bucket object ${PROXY_IGNITION_LOCATION_S3_URI}: put-object-acl succeeded"

generate_proxy_aws_template

EXPIRATION_DATE=$(date -d '4 hours' --iso=minutes --utc)
TAGS="Key=expirationDate,Value=${EXPIRATION_DATE}"

aws --region "${REGION}" cloudformation create-stack \
  --stack-name "${PROXY_NAME}" \
  --template-body "$(cat "${TEMP_DIR}/cluster_proxy_aws_template.yaml")" \
  --tags "${TAGS}" \
  --capabilities CAPABILITY_NAMED_IAM \
  --parameters \
  ParameterKey=ClusterName,ParameterValue="${PROXY_NAME}" \
  ParameterKey=VpcId,ParameterValue="${VPC_ID}" \
  ParameterKey=ProxyIgnitionLocation,ParameterValue="${PROXY_IGNITION_LOCATION_S3_URI}" \
  ParameterKey=InfrastructureName,ParameterValue="${INFRASTRUCTURE_NAME}" \
  ParameterKey=Ami,ParameterValue="${AMI}" \
  ParameterKey=PublicSubnet,ParameterValue="${PUBLIC_SUBNET_ID}" &

wait "$!"
echo "Created stack ${PROXY_NAME}"

aws --region "${REGION}" cloudformation wait stack-create-complete --stack-name "${PROXY_NAME}" &
wait "$!"
echo "Waited for stack"

echo ""
INSTANCE_ID=$(aws cloudformation describe-stacks --stack-name "${PROXY_NAME}" --query 'Stacks[].Outputs[?OutputKey == `ProxyId`].OutputValue' --region "${REGION}"  --output text)
echo "Created EC2 instance with id ${INSTANCE_ID} for proxy ${PROXY_NAME}"

echo ""
echo "Fetching IPs for EC2 instance ${INSTANCE_ID} for proxy ${PROXY_NAME}"
PRIVATE_PROXY_IP=$(aws  cloudformation describe-stacks --stack-name "${PROXY_NAME}" --query 'Stacks[].Outputs[?OutputKey == `ProxyPrivateIp`].OutputValue' --region "${REGION}" --output text)
PUBLIC_PROXY_IP=$(aws cloudformation describe-stacks --stack-name "${PROXY_NAME}" --query 'Stacks[].Outputs[?OutputKey == `ProxyPublicIp`].OutputValue' --region "${REGION}" --output text)
echo "EC2 instance ${INSTANCE_ID} PRIVATE_PROXY_IP: ${PRIVATE_PROXY_IP}"
echo "EC2 instance ${INSTANCE_ID} PUBLIC_PROXY_IP: ${PUBLIC_PROXY_IP}"

echo ""
echo "Generating URL for proxy ${PROXY_NAME}"
PROXY_URL="http://${PROXY_NAME}:${PASSWORD}@${PRIVATE_PROXY_IP}:3128"
# due to https://bugzilla.redhat.com/show_bug.cgi?id=1750650 we don't use a tls end point for squid

echo "Fetching existing cluster proxy specs"
oc get proxy cluster -o yaml
echo ""
echo "Patching cluster proxy specs httpProxy and httpsProxy with URL: ${PROXY_URL}"
oc patch proxy/cluster --type=merge -p '{"spec":{"httpProxy":"'"${PROXY_URL}"'","httpsProxy":"'"${PROXY_URL}"'","noProxy":".example.com"}}'
echo ""
echo "Fetching patched cluster proxy specs"
oc get proxy cluster -o yaml

echo ""
echo "Cleanup"
echo ""
echo "To remove updated proxy specs from cluster"
echo "    oc patch proxy/cluster --type=merge -p '{\"spec\":{\"httpProxy\":\"\", \"httpsProxy\":\"\", \"noProxy\":\"\"}}'"
echo ""
echo "To delete proxy cloudformation"
echo "    aws cloudformation delete-stack --stack-name ${PROXY_NAME} --region ${REGION}"
echo ""
echo "To check on the cloudformation deletion status"
echo "    aws cloudformation describe-stacks --stack-name ${PROXY_NAME} --query 'Stacks[0].StackStatus'"
echo ""
echo "To terminate proxy EC2 instance"
echo "    aws ec2 terminate-instances --instance-ids ${INSTANCE_ID} --region ${REGION}"
echo ""
echo "To delete proxy S3 bucket"
echo "    aws s3 rb s3://${PROXY_NAME} --force"
echo ""
echo "To delete local proxy ignition file"
echo "    rm -rf ${TEMP_DIR}/${PROXY_IGNITION_FILENAME}"
echo ""
echo "To delete local cloudformation template file"
echo "    rm -rf ${TEMP_DIR}/cluster_proxy_aws_template.yaml"
echo ""
