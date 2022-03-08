#!/bin/bash

# create-ptr-records.sh - Create PTR records in AWS Route53 for vSphere CI.
# For platform=none clusters the following ci-segments are reserved:
#  - ci-segment-56
#  - ci-segment-57
#  - ci-segment-58
#  - ci-segment-59
# each segment sits on a `/27` subnet (`192.168.x.1/27`) with DHCP range from
# `192.168.x.10` to `192.168.x.30`, where `x` accounts for the third octet and
# matches the number of the ci-segment, from 56 to 59 inclusive. DNS is provided
# by the VPC with server IP `10.0.0.2`.
#
# USAGE
#    create-ptr-records.sh
#
# PREREQUISITES
#    aws      to fetch VPC information and create hosted zone with record sets

# name of the VPC cloud formation stack
VSPHERE_VPC_STACK_NAME="vsphere-vpc"

# ci-segments for platform=none in vSphere
CI_SEGMENTS=(56 57 58 59)

# start and end IP addresses of the subnet
SUBNET_START=10
SUBNET_END=30

# createJSONRecordSets creates the JSON batch file with the PTR record sets
# for the given IP range.
function createJSONRecordSets () {
    batch_file=$1; third_octet=$2
    # init change_batch.json file content
    cat <<EOF > ${batch_file}
{
  "Comment": "PTR records for ci-segment-${third_octet}",
  "Changes": [
EOF
    # loop ip range
    for ip in $(seq $SUBNET_START $SUBNET_END); do
        # append record set
        cat <<EOF >> ${batch_file}
    {
      "Action": "CREATE",
      "ResourceRecordSet": {
        "Name": "${ip}.${third_octet}.168.192.in-addr.arpa",
        "Type": "PTR",
        "TTL": 172800,
        "ResourceRecords": [
          {
            "Value": "192.168.${third_octet}.${ip}"
          }
        ]
      }
    }$([[ $ip = $SUBNET_END ]] && echo "" || echo ",")
EOF
    done
    cat <<EOF >> ${batch_file}
  ]
}
EOF
}

# find the VPC ID
VPC_ID=$(aws cloudformation describe-stacks \
    --region $AWS_REGION \
    --stack-name $VSPHERE_VPC_STACK_NAME \
    | jq -r '.Stacks[0].Outputs[] | select(.OutputKey | contains("VpcId")).OutputValue') || {
    echo "Error getting vSphere VPC ID"
    exit 1
}

# create temp file for batch data
batch_file=$(mktemp)

# loop ci-segments
for third_octet in "${CI_SEGMENTS[@]}"; do
    echo "Processing ci-segment-$third_octet"
    echo "Creating private hosted zone associate to VPC $AWS_REGION/$VPC_ID"
    hosted_zone_id=$(aws route53 create-hosted-zone \
        --name "${third_octet}".168.192.in-addr.arpa \
        --hosted-zone-config PrivateZone=true,Comment="Hosted zone for VMC ci-segment-${third_octet}" \
        --caller-reference "$(uuidgen)" \
        --vpc VPCRegion="$AWS_REGION",VPCId="$VPC_ID" | jq -r '.HostedZone.Id') || {
            echo "Error creating hosted zone for ci-segment-${third_octet}"
            exit 1
        }
    echo "Created hosted zone $hosted_zone_id for ci-segment-${third_octet}"

    echo "Creating PTR record sets batch file for ci-segment-${third_octet}"
    createJSONRecordSets "${batch_file}" "${third_octet}"

    echo "Adding PTR record sets to hosted zone ${hosted_zone_id}"
    aws route53 change-resource-record-sets \
      --hosted-zone-id "${hosted_zone_id}" \
      --change-batch file://"${batch_file}" || {
        echo "Error creating PTR record sets for hosted zone ${hosted_zone_id} from batch file ${batch_file}"
        exit 1
      }
done

# cleanup
rm -f "${batch_file}"

# success
exit 0
