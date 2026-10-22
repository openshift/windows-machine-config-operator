#!/usr/bin/env python3
"""Safely create and monitor temporary WMCO QE rehearsal pull requests."""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import hashlib
import json
import os
import re
import secrets
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
from collections.abc import Iterable, Sequence
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

import yaml

UPSTREAM_REPOSITORY = "openshift/release"
UPSTREAM_REMOTE = "upstream"
FORK_REMOTE = "origin"
MAX_REHEARSALS = 25
LIVE_CONFIRMATION = "CREATE_TEMPORARY_DO_NOT_MERGE_PR"
CONFIG_RELATIVE = (
    "ci-operator/config/openshift/openshift-tests-private/"
    "openshift-openshift-tests-private-release-{version}__amd64-nightly.yaml"
)
JOBS_RELATIVE = (
    "ci-operator/jobs/openshift/openshift-tests-private/"
    "openshift-openshift-tests-private-release-{version}-periodics.yaml"
)
STEP_REGISTRY = Path("ci-operator/step-registry")
CONNECTED_CATALOG_REF = "openshift-windows-setup-wmco-konflux"
DISCONNECTED_CATALOG_REF = "openshift-windows-setup-wmco-konflux-disconnected"
SUPPORTED_CATALOG_REFS = {CONNECTED_CATALOG_REF, DISCONNECTED_CATALOG_REF}
OPERATORHUB_SUBSCRIBE_REF = "operatorhub-subscribe"
DISCONNECTED_INSTALL_REF = "openshift-windows-install-wmco"
PUBLIC_TEMPLATE_SOURCE = "openshift/release@0f72497967354022420becb86928c655a2b117c9"
CLOSURE_MANIFEST_SCHEMA = 1
CLOSURE_MANIFEST_IDENTITY = f"{PUBLIC_TEMPLATE_SOURCE}:wmco-qe-closure-v1"
PR_BASE_REF = "main"
TRUSTED_SCRIPT_TEMPLATES: dict[str, dict[str, str]] = {
    CONNECTED_CATALOG_REF: {
        "identity": f"{PUBLIC_TEMPLATE_SOURCE}:connected-producer-v1",
        "role": "connected-producer",
        "ref_path": (
            "ci-operator/step-registry/openshift/windows/setup-wmco-konflux/"
            "openshift-windows-setup-wmco-konflux-ref.yaml"
        ),
        "command_path": (
            "ci-operator/step-registry/openshift/windows/setup-wmco-konflux/"
            "openshift-windows-setup-wmco-konflux-commands.sh"
        ),
        "digest": "9378945efb5a4c4430f7f6c8bf40e9196981a1eaadd72e358211d30499d60fcb",
    },
    DISCONNECTED_CATALOG_REF: {
        "identity": f"{PUBLIC_TEMPLATE_SOURCE}:disconnected-producer-v1",
        "role": "disconnected-producer",
        "ref_path": (
            "ci-operator/step-registry/openshift/windows/"
            "setup-wmco-konflux-disconnected/"
            "openshift-windows-setup-wmco-konflux-disconnected-ref.yaml"
        ),
        "command_path": (
            "ci-operator/step-registry/openshift/windows/"
            "setup-wmco-konflux-disconnected/"
            "openshift-windows-setup-wmco-konflux-disconnected-commands.sh"
        ),
        "digest": "6d5d65bf3bd02e0d0eeff7b7ab1c00e0e64bd19e4c6cb95568e34b771f9e99ae",
    },
    OPERATORHUB_SUBSCRIBE_REF: {
        "identity": f"{PUBLIC_TEMPLATE_SOURCE}:connected-consumer-v1",
        "role": "connected-consumer",
        "ref_path": (
            "ci-operator/step-registry/operatorhub/subscribe/"
            "operatorhub-subscribe-ref.yaml"
        ),
        "command_path": (
            "ci-operator/step-registry/operatorhub/subscribe/"
            "operatorhub-subscribe-commands.sh"
        ),
        "digest": "62f8c00129bd1c086c8a7c6755012b90987088b7f0c9f0cc3c86dda392dfe788",
    },
    DISCONNECTED_INSTALL_REF: {
        "identity": f"{PUBLIC_TEMPLATE_SOURCE}:disconnected-consumer-v1",
        "role": "disconnected-consumer",
        "ref_path": (
            "ci-operator/step-registry/openshift/windows/install-wmco/"
            "openshift-windows-install-wmco-ref.yaml"
        ),
        "command_path": (
            "ci-operator/step-registry/openshift/windows/install-wmco/"
            "openshift-windows-install-wmco-commands.sh"
        ),
        "digest": "dc64c4c514d945c18bccc70861226cca3b5aea1953c1a1f9a9b431b0f455329d",
    },
}
# This inventory is generated only after manually reviewing the complete public
# command scripts at PUBLIC_TEMPLATE_SOURCE. It is deliberately literal: a
# source update requires a complete closure audit and a manifest version bump.
AUDITED_COMMAND_DIGESTS: dict[str, str] = {
    "ci-operator/step-registry/aws/deprovision/users-and-policies/aws-deprovision-users-and-policies-commands.sh": "eb151b0a0d322477bf1db48e05e84b15ec4fbbfaeaa966ff33c40476490b77cb",
    "ci-operator/step-registry/aws/deprovision/verification/aws-deprovision-verification-commands.sh": "fa5feadfc67aecf1ce259a14fd752738d527669d4fc55680269ebe6fb5900f26",
    "ci-operator/step-registry/aws/provision/iam-user/aws-provision-iam-user-commands.sh": "059f37a147e59485bda16e3e94d21b2dd8fa5f51a70a0c894f6e13b818a03bc7",
    "ci-operator/step-registry/azure/deprovision/sp-and-custom-role/azure-deprovision-sp-and-custom-role-commands.sh": "133a4bb0aa3aa34e1dc1c626c561a5c5616bda0f28771e6d0b9e0c8f1682e9d6",
    "ci-operator/step-registry/azure/provision/custom-role/azure-provision-custom-role-commands.sh": "962116176ec87099d24fdd32f70eff394a66c03da356b0aa671cd5d3e1572e9a",
    "ci-operator/step-registry/azure/provision/service-principal/minimal-permission/azure-provision-service-principal-minimal-permission-commands.sh": "c2a86f5a3bb84e405beb294e12da51d1a1f73d42b62db04bfa8fed86fabade29",
    "ci-operator/step-registry/clusterbot/wait/clusterbot-wait-commands.sh": "a2b41dd1cd6c56f01cf75b02452bef7cfe849605eeca1ab3f7a2ba401f49d1ac",
    "ci-operator/step-registry/cucushift/installer/check/azure/destroy/dns/cucushift-installer-check-azure-destroy-dns-commands.sh": "4f04d5bc75c43bce92f1216b322f208a79534f0017ef4b27ca3885e425e19e09",
    "ci-operator/step-registry/cucushift/installer/check/capability/cucushift-installer-check-capability-commands.sh": "f72e3550f4e3d28b484a9090f3d1b5be1518adfe3142f8c01540abd99ae6d43f",
    "ci-operator/step-registry/cucushift/installer/check/compact/cucushift-installer-check-compact-commands.sh": "5d86b40ecdcd616cb5723c6947f4357f51376e126500050a2ccc4179c07a8713",
    "ci-operator/step-registry/cucushift/installer/check/control-plane-machinesets/cucushift-installer-check-control-plane-machinesets-commands.sh": "56e7073e37aceabfa9d03f16262e769c938fa74cf729268fa597df7f7c2c5742",
    "ci-operator/step-registry/cucushift/installer/check/fips/cucushift-installer-check-fips-commands.sh": "2731c94595c6840eab507959e1eb2845096f62e19f637f18c8502417cf98c574",
    "ci-operator/step-registry/cucushift/installer/check/info/cucushift-installer-check-info-commands.sh": "7cc8b4ad56b324006451e0f24108273c06799cc48688046d2b67b5dad423f7d9",
    "ci-operator/step-registry/cucushift/installer/check/operators/cucushift-installer-check-operators-commands.sh": "b78e3186a85c2114a4c471dcfa5c8cb6e6268345e67e09dfbea412d7ec3edfe7",
    "ci-operator/step-registry/cucushift/installer/check/realtime/cucushift-installer-check-realtime-commands.sh": "931ff050bda529773ae167227e3674358fa3e159dfb958706759e02ad3a390c9",
    "ci-operator/step-registry/cucushift/installer/reportportal-marker/cucushift-installer-reportportal-marker-commands.sh": "726fe4b77b66507933672a7617f3878a78831875b1c27da8d67df6c7e0d1bbe2",
    "ci-operator/step-registry/cucushift/winc/prepare/cucushift-winc-prepare-commands.sh": "5929920953dfc3d9dc9c17f06680a9c71aa38be97b3fe2ee9d0129927e34422c",
    "ci-operator/step-registry/gather/audit-logs/gather-audit-logs-commands.sh": "706b0c7e8c3470a55903b5905d2f52138973cb3986a05c029580bd91bc096a90",
    "ci-operator/step-registry/gather/aws-console/gather-aws-console-commands.sh": "c31499cbe8d281d16768d99cf292a25ccd1c0fc58b0766170de49bff64893665",
    "ci-operator/step-registry/gather/azure-resource/gather-azure-resource-commands.sh": "18956a56ee2e762d59fb1f8d0f87620dac6cff4db998b676e8abdd8a1bef5926",
    "ci-operator/step-registry/gather/core-dump/gather-core-dump-commands.sh": "25ded21bf200a67bae8f76443bb73cc87258f7fc5dcff01b898f89b9a1e48133",
    "ci-operator/step-registry/gather/extra/gather-extra-commands.sh": "3eab0f0767f58a1d8cb04849fbe3326eca6be1e15e4f6bfe84323d64c4c8fdbc",
    "ci-operator/step-registry/gather/gcp-console/gather-gcp-console-commands.sh": "5d0be8121a0c3fccc90dd4ea99b286b1c60a3b744f472da61c700a9139a3d860",
    "ci-operator/step-registry/gather/mirror-registry/gather-mirror-registry-commands.sh": "ceb1d7e1714f87860652262892546bf2e0601876b615098003b44731061aa7b1",
    "ci-operator/step-registry/gather/must-gather/gather-must-gather-commands.sh": "25938b5edacdb7aaa4eadaee9c460fe6ecb3a55d3c083e655a2459bd5e6f3250",
    "ci-operator/step-registry/gather/network/gather-network-commands.sh": "804c1ff41c8e52db71661edcc23c7fcc2c8db635c89b953f4682553b04013edd",
    "ci-operator/step-registry/ignition/bastionhost/ignition-bastionhost-commands.sh": "180117294e6313246c175801fa2e9ee5552c5eff9164fde35307b11428f4c5e0",
    "ci-operator/step-registry/ipi/conf/aws/byo-ipv4-pool-public/ipi-conf-aws-byo-ipv4-pool-public-commands.sh": "fdeedf5b4fef9f4823b0ae57387d1b213b9e85f5a6b90012bfa7e34d520c1708",
    "ci-operator/step-registry/ipi/conf/aws/ipi-conf-aws-commands.sh": "0d1fadf9604faac5f4c0a3f79b5c493d6e2c8e1eabbc0b4266449da20eae0879",
    "ci-operator/step-registry/ipi/conf/aws/usage-info/ipi-conf-aws-usage-info-commands.sh": "e368c176fa48ddaf1f4f67f09825d0d0a62df5e8aa54ed6fb8cc5a78493d646f",
    "ci-operator/step-registry/ipi/conf/aws/user-min-permissions/ipi-conf-aws-user-min-permissions-commands.sh": "5652b8bc1e53fa1a7346428be1a2945392606c0799fdbc0c95f174486a5348c1",
    "ci-operator/step-registry/ipi/conf/aws/windows-machineset/ipi-conf-aws-windows-machineset-commands.sh": "391dfe9d7a0d8b82264e0e084c4000d756f74b910a07d114dbee5338383adb76",
    "ci-operator/step-registry/ipi/conf/azure/ipi-conf-azure-commands.sh": "5d36cb1485a20e9346be3c42406a50c77b06eccc91340ac24a1d4b8d0a509500",
    "ci-operator/step-registry/ipi/conf/azure/windows-machineset/ipi-conf-azure-windows-machineset-commands.sh": "efae53bd04a205c7bbb8ccbaf34b6bb85d695c547dded8297ae77094a90ebe6e",
    "ci-operator/step-registry/ipi/conf/gcp/ipi-conf-gcp-commands.sh": "b8691e772cd058b32418503c2890a41d88020570fa2620fb57acf15f01536636",
    "ci-operator/step-registry/ipi/conf/gcp/windows-machineset/ipi-conf-gcp-windows-machineset-commands.sh": "4a9b5f9e69f44df8b7a519e1f8b3c562b2c55284961e238c5994c916a9ff3c60",
    "ci-operator/step-registry/ipi/conf/gcp/zones/ipi-conf-gcp-zones-commands.sh": "f7e6c652f8a012518fbd28661bc696be1f28337863e4df7cf8fa94a13cfb3b3b",
    "ci-operator/step-registry/ipi/conf/ipi-conf-commands.sh": "4633ac703fd371a9213b76a0154a76df36e3dfbd0f21a611df57b55b9a01893d",
    "ci-operator/step-registry/ipi/conf/mirror/ipi-conf-mirror-commands.sh": "7519fbe671a9960eb68d00825cf1e4e311e72cb9ae1010465ded65cd490c6e27",
    "ci-operator/step-registry/ipi/conf/telemetry/ipi-conf-telemetry-commands.sh": "8a12bf530181a69fd76446f771882f51b0f42c054511e1987a72bb08ded5fe96",
    "ci-operator/step-registry/ipi/conf/vsphere/check/ipi-conf-vsphere-check-commands.sh": "425d0bb4c0a59bdca11053073705a5584e32db3fe44e268dc24f780c32c31416",
    "ci-operator/step-registry/ipi/conf/vsphere/check/vcm/ipi-conf-vsphere-check-vcm-commands.sh": "4b1e19cd38627ed73f4a70b5eb94541bcf7ed82275bd851d2f1130b5dfa2456d",
    "ci-operator/step-registry/ipi/conf/vsphere/dns/ipi-conf-vsphere-dns-commands.sh": "460eee9df6a2d9313f714bfde37955e49f73599cf2fe1fd3d445ff79122cbabf",
    "ci-operator/step-registry/ipi/conf/vsphere/ipi-conf-vsphere-commands.sh": "92de0184a31ca4295717d2513103dd1dce07f3b1f33435aa0f8ac3355097dee7",
    "ci-operator/step-registry/ipi/conf/vsphere/multi-nic/secondary-network/ipi-conf-vsphere-multi-nic-secondary-network-commands.sh": "ae971edd674ef2dfe5a78fabd7329f8b46dc33d78e9afee2f2725c5edf1e6495",
    "ci-operator/step-registry/ipi/conf/vsphere/vcm/ipi-conf-vsphere-vcm-commands.sh": "aa7701f8a411505662df088ad7f9b2dbb0fade5c8ebe1036f7d75c814d142a00",
    "ci-operator/step-registry/ipi/conf/vsphere/vips/ipi-conf-vsphere-vips-commands.sh": "5491064ca129e10fbe4dbfb7911cd0e7ce4a2765522f1b92a3c309f4f7d9a131",
    "ci-operator/step-registry/ipi/conf/vsphere/vips/vcm/ipi-conf-vsphere-vips-vcm-commands.py": "e1b23aca3e78433150cca507b15a75f990d8174ee2821523729f7a08cce0c6b9",
    "ci-operator/step-registry/ipi/conf/vsphere/windows-machineset/ipi-conf-vsphere-windows-machineset-commands.sh": "aeb88dc9df0fadebf75c3c86c9a8c8eec685c9b804e7d141af412fc1360a5575",
    "ci-operator/step-registry/ipi/deprovision/deprovision/ipi-deprovision-deprovision-commands.sh": "a5519e1ae4e894bc743748c4747ba49e90d7a7c02111f2327df8bbcf75969b91",
    "ci-operator/step-registry/ipi/deprovision/vsphere/diags/ipi-deprovision-vsphere-diags-commands.sh": "166480538be7ba0f527b578214775b63c2684f7ca0c577c1c337db2b0f4d4c66",
    "ci-operator/step-registry/ipi/deprovision/vsphere/dns/ipi-deprovision-vsphere-dns-commands.sh": "2fd07b807891dbf0978ed040e60870193ee77a4ec0e4262afc35e1a952fc04e2",
    "ci-operator/step-registry/ipi/deprovision/vsphere/lb/ipi-deprovision-vsphere-lb-commands.sh": "084709168956e0931d0672d33e8e130555eef79e9d9a25b8858cfc0fa98b140f",
    "ci-operator/step-registry/ipi/deprovision/vsphere/lease/ipi-deprovision-vsphere-lease-commands.sh": "bdc7724ebcf8d989a9a11549743573f4d41046cb1c8adde49932fc4b3dd556bf",
    "ci-operator/step-registry/ipi/install-times-collection/ipi-install-times-collection-commands.sh": "a8d84b7867e4a998b8aa714c244e04f94dea62f2c641fa2e7890467340886315",
    "ci-operator/step-registry/ipi/install/hosted-loki/ipi-install-hosted-loki-commands.sh": "4f7867b13098835d17145e207d33c14b5ee36ae920688d79263186a6a8e70a55",
    "ci-operator/step-registry/ipi/install/install/ipi-install-install-commands.sh": "27c7600e0447b071f38e0bac1300e5ede232930ef850fdac2675145728bd1368",
    "ci-operator/step-registry/ipi/install/monitoringpvc/ipi-install-monitoringpvc-commands.sh": "78bff371f8fa2daf8ea3f3d939b8380ee5e04b1ea15a750d50039047c5879e8e",
    "ci-operator/step-registry/ipi/install/rbac/ipi-install-rbac-commands.sh": "dadedf90ded72051014f4ac0684904cd58362c56771ce82f7d181c0dccbe396b",
    "ci-operator/step-registry/ipi/install/vsphere/registry/ipi-install-vsphere-registry-commands.sh": "a4739d8908e1d27dc63f48be7da97a947d1bc9933cff362a978cb9f24e20c917",
    "ci-operator/step-registry/mirror-images/by-oc-adm-in-bastion/mirror-images-by-oc-adm-in-bastion-commands.sh": "28bcec72a90ce4c9e0ef63c1cf2d9cef4be325f1fb82b3a1245589bb71b3b3f5",
    "ci-operator/step-registry/mirror-images/by-oc-adm/mirror-images-by-oc-adm-commands.sh": "3135ce5b6cfa6ce1779ab6139c22e646b51378298d84e45021cf40b69d73d2f5",
    "ci-operator/step-registry/mirror-images/by-oc-mirror/conf-mirror/mirror-images-by-oc-mirror-conf-mirror-commands.sh": "3585046e686c85c6828b66fc517174b1f9624ac580d5dfa338460c2abbb1c4c6",
    "ci-operator/step-registry/mirror-images/by-oc-mirror/mirror-images-by-oc-mirror-commands.sh": "dd808fd064a6cc429c2cb0b3f6db70cf4d51339ea464b5ab359b59d544503d20",
    "ci-operator/step-registry/mirror-images/check-registry-service/mirror-images-check-registry-service-commands.sh": "635cb6411201e9c5b62e90ec0f577276a5ed8a601ddb17c65740b83b82467c10",
    "ci-operator/step-registry/mirror-images/tag-images/mirror-images-tag-images-commands.sh": "e72061cd4564351217ba4c4aca6611fc4a3eb67f149c5af9235f24955b503148",
    "ci-operator/step-registry/multiarch/validate-nodes/multiarch-validate-nodes-commands.sh": "291a3008fc07b1564e65f4511bc05f8eee1459b5b8dc46b5471d9034bd5a8e46",
    "ci-operator/step-registry/nodes/readiness/nodes-readiness-commands.sh": "794b79c0e3335c4442abe7af80979e4b97fa7bae6339363a1a5153e593517d34",
    "ci-operator/step-registry/openshift-tests-extension/admission-crd-install/openshift-tests-extension-admission-crd-install-commands.sh": "a8b11b10fd221eae28ba264e1ac18ac00d9b2b78685d0b5be7879d1e51751d25",
    "ci-operator/step-registry/openshift/cluster-bot/rbac/openshift-cluster-bot-rbac-commands.sh": "437179864027d15a9b050808238b96a5f417a14c5eace9bdab96e7c5aaeb7e9e",
    "ci-operator/step-registry/openshift/windows/install-wmco/openshift-windows-install-wmco-commands.sh": "dc64c4c514d945c18bccc70861226cca3b5aea1953c1a1f9a9b431b0f455329d",
    "ci-operator/step-registry/openshift/windows/setup-wmco-konflux-disconnected/openshift-windows-setup-wmco-konflux-disconnected-commands.sh": "6d5d65bf3bd02e0d0eeff7b7ab1c00e0e64bd19e4c6cb95568e34b771f9e99ae",
    "ci-operator/step-registry/openshift/windows/setup-wmco-konflux/openshift-windows-setup-wmco-konflux-commands.sh": "9378945efb5a4c4430f7f6c8bf40e9196981a1eaadd72e358211d30499d60fcb",
    "ci-operator/step-registry/operatorhub/subscribe/operatorhub-subscribe-commands.sh": "62f8c00129bd1c086c8a7c6755012b90987088b7f0c9f0cc3c86dda392dfe788",
    "ci-operator/step-registry/ovn/conf/hybrid-manifest-with-custom-vxlan-port/ovn-conf-hybrid-manifest-with-custom-vxlan-port-commands.sh": "e81c64a6ce968a33acbc8b8884657795720e16eeae3b28b08e24822b105a1f5d",
    "ci-operator/step-registry/ovn/conf/hybrid-manifest/ovn-conf-hybrid-manifest-commands.sh": "52b6e80271ba1c8359d6664bd7a0767f69f155ecc8325054d5f7211facd1e79f",
    "ci-operator/step-registry/ovn/conf/ovn-conf-commands.sh": "a6c758b85805c96dd234982f97878e9f8388c3e4dab60f2966b4c8d44c2f85b0",
    "ci-operator/step-registry/send-results/to-reportportal/send-results-to-reportportal-commands.sh": "eca0cb73ea8c6b7b9c6ec49e168308175eeea9c2a259cb2430ce5708c7f5116b",
    "ci-operator/step-registry/set-sample-operator/disconnected/set-sample-operator-disconnected-commands.sh": "3600348d3496f4bb9b83537c70ecd79cbbf86ac415812a94acd4f74ce8e3ebcc",
    "ci-operator/step-registry/ssh-bastion/ssh-bastion-commands.sh": "c287f0729cd3692ddeae415d66808f5555312e7c276307f4cbece026ecfbb5c5",
    "ci-operator/step-registry/vsphere/deprovision/bastionhost/vsphere-deprovision-bastionhost-commands.sh": "eb2aaa9cc4b186811e12e05767a2c09078774171fb478eae74bf2b270fe98fd9",
    "ci-operator/step-registry/vsphere/deprovision/customized-resourcepool-check/vsphere-deprovision-customized-resourcepool-check-commands.sh": "01c1e3866598dac542bd739dced78a3fe4c7a86b60ba516de933663fecc019a0",
    "ci-operator/step-registry/vsphere/provision/bastionhost/vsphere-provision-bastionhost-commands.sh": "7f0891a3828f94be3066ab722071e7806bbd2636b3af8b9df29b43d1992151c7",
    "ci-operator/step-registry/windows/conf/operator/windows-conf-operator-commands.sh": "c0572a13a5087f5c49f270168b6ba66616a300222038d7edec338dc66d7fcc64",
    "ci-operator/step-registry/windows/e2e/operator/test/mirror-images/windows-e2e-operator-test-mirror-images-commands.sh": "1f1bfcc124550dcfe31ccb769dcec7895f652fb7b75a123ad6c135a8192809f5",
}

# Digests cover typed dependency edges, including workflow phase, ancestry,
# list position, dependency kind/name/path, and allowed companion values.
AUDITED_WORKFLOW_TOPOLOGIES: dict[str, str] = {
    "cucushift-installer-rehearse-aws-ipi-ovn-winc": "93eb5983f547c76f365e962c485861d1a788f77eb4d380b8e3780065e179d4ab",
    "cucushift-installer-rehearse-azure-ipi-ovn-winc": "6d9bfec0722e0639361fe823851885f2d772030b7e998d34acf84d9c75638c0d",
    "cucushift-installer-rehearse-gcp-ipi-ovn-winc": "d043a8279645be20aeb5620fd3379693649297a8164e63ce2788c586d08045b1",
    "cucushift-installer-rehearse-vsphere-ipi-disconnected-ovn-winc": "2c0210609c95943267d0ab3de8e2c9d21736b628126a2fa6f7f8951d23a0dc14",
}

STATE_SCHEMA = 3
OCP_VERSION_RE = re.compile(r"^(?:4|5)\.[0-9]+$")
RUN_ID_RE = re.compile(r"^[0-9]{8}t[0-9]{6}z-[0-9a-f]{8}$")
WMCO_JOB_RE = re.compile(
    r"^[a-z0-9]+(?:-[a-z0-9]+)*-winc(?:-[a-z0-9]+)*-f[0-9]+"
    r"(?:-[a-z0-9]+)*$"
)
AS_LINE_RE = re.compile(r"(?m)^- as: (?P<name>[a-z0-9][a-z0-9-]*)$")
STREAM_MARKER_RE = re.compile(r"(?:^|-)winc-(?:zstream|ystream)(?:-|$)")
JOB_OVERRIDE_RE = re.compile(
    r"(?im)^\s+(?:WMCO_INDEX_IMAGE|SUB_SOURCE|SUB_PACKAGE|SUB_CHANNEL|"
    r"INDEX_IMAGE|OPERATOR_INDEX|OPERATORS_INDEX|OO_INDEX|OLM_CATALOG|"
    r"CATALOG_SOURCE)\s*:"
)
PROW_HOSTS = {
    "prow.ci.openshift.org",
    "qe-private-deck-ci.apps.ci.l2s4.p1.openshiftapps.com",
}
NOTIFIER_LOGIN = "openshift-merge-bot[bot]"
NOTIFIER_APP_SLUG = "openshift-merge-bot"
# This is the public GitHub service account that publishes OpenShift CI status,
# not a credential. Keep the identity literal so the trust boundary is explicit.
AUTHORITATIVE_PROW_ACCOUNT = "openshift-ci[bot]"
PROW_PUBLISHER_APP_SLUG = "openshift-ci"


class SafetyError(RuntimeError):
    """Raised when an invariant cannot be proven."""


class StrictSafeLoader(yaml.SafeLoader):
    """Safe YAML loader that rejects ambiguous mapping construction."""


def _construct_strict_mapping(
    loader: StrictSafeLoader, node: yaml.nodes.MappingNode, deep: bool = False
) -> dict[Any, Any]:
    if not isinstance(node, yaml.nodes.MappingNode):
        raise yaml.constructor.ConstructorError(
            None, None, "expected a mapping node", node.start_mark
        )
    keys: set[Any] = set()
    for key_node, _ in node.value:
        if key_node.tag == "tag:yaml.org,2002:merge":
            raise yaml.constructor.ConstructorError(
                None,
                None,
                "YAML merge keys are not supported",
                key_node.start_mark,
            )
        key = loader.construct_object(key_node, deep=deep)
        try:
            duplicate = key in keys
        except TypeError as exc:
            raise yaml.constructor.ConstructorError(
                None,
                None,
                "mapping keys must be hashable scalars",
                key_node.start_mark,
            ) from exc
        if duplicate:
            raise yaml.constructor.ConstructorError(
                None, None, "duplicate mapping key", key_node.start_mark
            )
        keys.add(key)
    return yaml.SafeLoader.construct_mapping(loader, node, deep=deep)


StrictSafeLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _construct_strict_mapping
)


def strict_yaml_load(text: str, *, context: str) -> Any:
    """Load safety-relevant YAML without duplicate or merge-key ambiguity."""
    try:
        loader = StrictSafeLoader(text)
        try:
            return loader.get_single_data()
        finally:
            loader.dispose()
    except yaml.YAMLError as exc:
        raise SafetyError(f"{context} is invalid or ambiguous YAML") from exc


class CommandError(SafetyError):
    """Raised when a local or read-only command fails."""

    def __init__(self, args: Sequence[str], returncode: int, stderr: str):
        command = " ".join(args)
        super().__init__(f"command failed ({returncode}): {command}: {stderr.strip()}")
        self.args_run = list(args)
        self.returncode = returncode
        self.stderr = stderr


@dataclasses.dataclass(frozen=True)
class CommandResult:
    stdout: str
    stderr: str
    returncode: int


class Runner:
    """Run argument-vector commands without a shell."""

    def run(
        self,
        args: Sequence[str],
        *,
        cwd: Path,
        check: bool = True,
        input_text: str | None = None,
    ) -> CommandResult:
        process = subprocess.run(
            list(args),
            cwd=cwd,
            input=input_text,
            text=True,
            capture_output=True,
            check=False,
        )
        result = CommandResult(process.stdout, process.stderr, process.returncode)
        if check and process.returncode != 0:
            raise CommandError(args, process.returncode, process.stderr)
        return result


@dataclasses.dataclass(frozen=True)
class JobPlan:
    version: str
    stream: str
    config_path: Path
    generated_path: Path
    discovered: tuple[str, ...]
    selected: tuple[str, ...]
    renamed: dict[str, str]
    workflows: dict[str, str]
    base_sha: str
    payload_definition: str
    catalog_paths: dict[str, str]
    catalog_templates: dict[str, tuple[dict[str, str], ...]]
    closure_manifests: dict[str, dict[str, str]]
    safety_inputs: dict[str, str]

    def __post_init__(self) -> None:
        if not self.selected:
            raise SafetyError("cannot plan an empty rehearsal job set")
        if len(self.selected) > MAX_REHEARSALS:
            raise SafetyError(
                f"{len(self.selected)} jobs exceed the authoritative /pj-rehearse "
                f"max limit of {MAX_REHEARSALS}"
            )

    @property
    def expected_prow_jobs(self) -> tuple[str, ...]:
        prefix = (
            "periodic-ci-openshift-openshift-tests-private-release-"
            f"{self.version}-amd64-nightly-"
        )
        return tuple(prefix + self.renamed[name] for name in self.selected)

    @property
    def digest(self) -> str:
        material = {
            "base_sha": self.base_sha,
            "catalog_paths": self.catalog_paths,
            "catalog_templates": self.catalog_templates,
            "closure_manifests": self.closure_manifests,
            "command": rehearsal_command(len(self.selected)),
            "expected_prow_jobs": self.expected_prow_jobs,
            "payload_definition": self.payload_definition,
            "renamed": self.renamed,
            "safety_inputs": self.safety_inputs,
            "selected": self.selected,
            "stream": self.stream,
            "version": self.version,
            "workflows": self.workflows,
        }
        encoded = json.dumps(material, sort_keys=True, separators=(",", ":")).encode()
        return hashlib.sha256(encoded).hexdigest()


def validate_ocp_version(version: str) -> str:
    match = OCP_VERSION_RE.fullmatch(version)
    if match is None:
        raise SafetyError(
            f"invalid OCP minor release {version!r}; expected values such as 4.21 or 5.0"
        )
    major, minor = version.split(".", maxsplit=1)
    canonical = f"{int(major)}.{int(minor)}"
    if canonical != version:
        raise SafetyError("OCP minor release must use canonical decimal notation")
    return canonical


def validate_stream(stream: str) -> str:
    normalized = stream.lower().removesuffix("-stream").removesuffix("stream")
    if normalized == "y":
        return "y"
    if normalized == "z":
        return "z"
    raise SafetyError("stream must be 'z', 'z-stream', 'y', or 'y-stream'")


def rehearsal_pr_title(version: str, stream: str) -> str:
    return (
        f"DEBUG Do not merge: OCP {validate_ocp_version(version)} "
        f"{validate_stream(stream).upper()} stream WMCO QE rehearsals"
    )


def validate_run_id(run_id: str) -> str:
    if RUN_ID_RE.fullmatch(run_id) is None:
        raise SafetyError("run ID is malformed")
    stamp, nonce = run_id.rsplit("-", maxsplit=1)
    try:
        parsed_stamp = dt.datetime.strptime(stamp, "%Y%m%dt%H%M%Sz").replace(
            tzinfo=dt.timezone.utc
        )
    except ValueError as exc:
        raise SafetyError("run ID is malformed") from exc
    canonical_stamp = (
        f"{parsed_stamp.year:04d}{parsed_stamp.month:02d}{parsed_stamp.day:02d}t"
        f"{parsed_stamp.hour:02d}{parsed_stamp.minute:02d}{parsed_stamp.second:02d}z"
    )
    return f"{canonical_stamp}-{int(nonce, 16):08x}"


def confined_path(root: Path, relative: Path, *, context: str) -> Path:
    """Resolve a relative path and require it to remain below its intended root."""
    if relative.is_absolute() or ".." in relative.parts:
        raise SafetyError(f"{context} path is not relative to its intended root")
    resolved_root = root.resolve()
    resolved_path = (resolved_root / relative).resolve()
    try:
        resolved_path.relative_to(resolved_root)
    except ValueError as exc:
        raise SafetyError(f"{context} path escapes its intended root") from exc
    return resolved_path


def new_run_id() -> str:
    stamp = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dt%H%M%Sz")
    return f"{stamp}-{secrets.token_hex(4)}"


def discover_wmco_jobs(config_text: str) -> tuple[str, ...]:
    config = strict_yaml_load(config_text, context="release config")
    tests = config.get("tests") if isinstance(config, dict) else None
    if not isinstance(tests, list):
        raise SafetyError("release config has no structured tests list")
    jobs: list[str] = []
    for entry in tests:
        if not isinstance(entry, dict):
            raise SafetyError("release config contains a malformed test entry")
        name = str(entry.get("as") or "")
        if (
            WMCO_JOB_RE.fullmatch(name)
            and isinstance(entry.get("cron"), str)
            and entry["cron"].strip()
            and "debug" not in name.split("-")
        ):
            jobs.append(name)
    if not jobs:
        raise SafetyError("no matching WMCO QE periodic jobs were discovered")
    if len(jobs) != len(set(jobs)):
        raise SafetyError("duplicate WMCO QE as values make selection ambiguous")
    already_renamed = [name for name in jobs if STREAM_MARKER_RE.search(name)]
    if already_renamed:
        raise SafetyError(
            "stream-renamed jobs already exist; refusing a duplicate trigger: "
            + ", ".join(sorted(already_renamed))
        )
    return tuple(jobs)


def select_jobs(jobs: Iterable[str], stream: str) -> tuple[str, ...]:
    stream = validate_stream(stream)
    ordered = tuple(jobs)
    if stream == "y":
        selected = ordered
    else:
        allowed_platforms = {"aws", "gcp", "azure", "vsphere"}
        selected = tuple(
            name
            for name in ordered
            if name.split("-", 1)[0] in allowed_platforms
            and any(token in {"ipi", "upi"} for token in name.split("-"))
            and name.split("-", 1)[0] != "nutanix"
        )
    if not selected:
        raise SafetyError(f"no jobs match the {stream}-stream selection policy")
    if stream == "z" and any(name.startswith("nutanix-") for name in selected):
        raise SafetyError("Nutanix must not be selected by default for z-stream")
    return selected


def renamed_job(name: str, stream: str) -> str:
    marker = f"{validate_stream(stream)}stream"
    parts = name.split("-")
    indices = [index for index, part in enumerate(parts) if part == "winc"]
    if len(indices) != 1:
        raise SafetyError(f"job name does not contain exactly one winc token: {name}")
    index = indices[0]
    return "-".join(parts[: index + 1] + [marker] + parts[index + 1 :])


def top_level_test_blocks(config_text: str) -> dict[str, str]:
    matches = list(AS_LINE_RE.finditer(config_text))
    blocks: dict[str, str] = {}
    for index, match in enumerate(matches):
        end = (
            matches[index + 1].start() if index + 1 < len(matches) else len(config_text)
        )
        blocks[match.group("name")] = config_text[match.start() : end]
    return blocks


def workflow_for_job(block: str, job: str) -> str:
    workflows = re.findall(r"(?m)^\s+workflow: ([a-z0-9-]+)$", block)
    if len(workflows) != 1:
        raise SafetyError(
            f"expected exactly one workflow for {job}, found {len(workflows)}"
        )
    if JOB_OVERRIDE_RE.search(block):
        raise SafetyError(f"job {job} contains a catalog or subscription override")
    return workflows[0]


def unique_registry_file(root: Path, name: str, kind: str) -> Path:
    suffix = f"{name}-{kind}.yaml"
    matches = sorted((root / STEP_REGISTRY).glob(f"**/{suffix}"))
    if len(matches) != 1:
        raise SafetyError(
            f"expected one step-registry {kind} for {name}, found {len(matches)}"
        )
    return matches[0]


DEPENDENCY_KINDS = ("ref", "chain", "workflow")
DEPENDENCY_NAME_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")
DEPENDENCY_COMPANIONS = {"env"}
WORKFLOW_STEP_KEYS = {"cluster_profile", "env", "pre", "test", "post"}
REGISTRY_NODE_KEYS = {
    "workflow": {"as", "documentation", "steps"},
    "chain": {"as", "documentation", "env", "steps"},
    "ref": {
        "as",
        "best_effort",
        "cli",
        "commands",
        "credentials",
        "dependencies",
        "documentation",
        "env",
        "from",
        "from_image",
        "grace_period",
        "optional_on_success",
        "resources",
        "run_as_script",
        "timeout",
    },
}


@dataclasses.dataclass(frozen=True)
class RegistryEdge:
    parent_kind: str
    parent_name: str
    parent_path: str
    phase: str
    ancestry: tuple[str, ...]
    position: int
    kind: str
    name: str
    path: str
    companions: str

    def material(self) -> dict[str, Any]:
        return dataclasses.asdict(self)


@dataclasses.dataclass(frozen=True)
class RegistryGraph:
    workflow: str
    nodes: tuple[tuple[str, str, Path], ...]
    edges: tuple[RegistryEdge, ...]
    refs: tuple[tuple[str, Path, str, tuple[str, ...], int], ...]

    @property
    def topology_digest(self) -> str:
        material = {
            "schema": CLOSURE_MANIFEST_SCHEMA,
            "workflow": self.workflow,
            "nodes": [
                {
                    "kind": kind,
                    "name": name,
                    "path": str(path),
                }
                for kind, name, path in self.nodes
            ],
            "edges": [edge.material() for edge in self.edges],
        }
        encoded = json.dumps(material, sort_keys=True, separators=(",", ":")).encode()
        return hashlib.sha256(encoded).hexdigest()


def _registry_document(text: str, *, context: str) -> tuple[str, dict[str, Any]]:
    document = strict_yaml_load(text, context=context)
    if not isinstance(document, dict) or len(document) != 1:
        raise SafetyError(f"{context} must contain exactly one registry node")
    kind, node = next(iter(document.items()))
    if kind not in DEPENDENCY_KINDS or not isinstance(node, dict):
        raise SafetyError(f"{context} has an unsupported registry node shape")
    unknown = set(node).difference(REGISTRY_NODE_KEYS[kind])
    if unknown:
        raise SafetyError(
            f"{context} {kind} has unsupported keys: "
            + ", ".join(sorted(str(key) for key in unknown))
        )
    return kind, node


def _dependency_entry(entry: Any, *, context: str) -> tuple[str, str, dict[str, Any]]:
    if not isinstance(entry, dict):
        raise SafetyError(f"{context} dependency must be a mapping")
    dependency_keys = [kind for kind in DEPENDENCY_KINDS if kind in entry]
    if len(dependency_keys) != 1:
        raise SafetyError(
            f"{context} dependency must have exactly one ref/chain/workflow key"
        )
    kind = dependency_keys[0]
    unknown = set(entry).difference({kind}, DEPENDENCY_COMPANIONS)
    if unknown:
        raise SafetyError(
            f"{context} dependency has unsupported companion keys: "
            + ", ".join(sorted(str(key) for key in unknown))
        )
    name = entry[kind]
    if not isinstance(name, str) or not DEPENDENCY_NAME_RE.fullmatch(name):
        raise SafetyError(f"{context} dependency has an invalid {kind} name")
    companions = {key: entry[key] for key in sorted(DEPENDENCY_COMPANIONS & set(entry))}
    if "env" in companions and not isinstance(companions["env"], dict):
        raise SafetyError(f"{context} dependency env companion must be a mapping")
    return kind, name, companions


def _node_dependencies(
    kind: str, node: dict[str, Any], *, context: str
) -> tuple[tuple[str, int, str, str, dict[str, Any]], ...]:
    if kind == "ref":
        return ()
    steps = node.get("steps")
    if kind == "workflow":
        if not isinstance(steps, dict):
            raise SafetyError(f"{context} workflow steps must be a mapping")
        unknown = set(steps).difference(WORKFLOW_STEP_KEYS)
        if unknown:
            raise SafetyError(
                f"{context} workflow steps have unsupported keys: "
                + ", ".join(sorted(str(key) for key in unknown))
            )
        if "env" in steps and not isinstance(steps["env"], dict):
            raise SafetyError(f"{context} workflow steps.env must be a mapping")
        if "cluster_profile" in steps and not isinstance(steps["cluster_profile"], str):
            raise SafetyError(f"{context} workflow cluster_profile must be a string")
        dependencies: list[tuple[str, int, str, str, dict[str, Any]]] = []
        for phase in ("pre", "test", "post"):
            entries = steps.get(phase, [])
            if not isinstance(entries, list):
                raise SafetyError(f"{context} workflow {phase} must be a list")
            for position, entry in enumerate(entries):
                child_kind, child_name, companions = _dependency_entry(
                    entry, context=f"{context} {phase}[{position}]"
                )
                dependencies.append(
                    (phase, position, child_kind, child_name, companions)
                )
        return tuple(dependencies)
    if not isinstance(steps, list):
        raise SafetyError(f"{context} chain steps must be a list")
    return tuple(
        (
            "",
            position,
            *_dependency_entry(entry, context=f"{context} steps[{position}]"),
        )
        for position, entry in enumerate(steps)
    )


def registry_dependencies(text: str) -> tuple[tuple[str, str], ...]:
    """Return structurally validated dependencies, independent of YAML style."""
    kind, node = _registry_document(text, context="step-registry document")
    return tuple(
        (dependency_kind, dependency_name)
        for _, _, dependency_kind, dependency_name, _ in _node_dependencies(
            kind, node, context=f"step-registry {kind}"
        )
    )


def registry_closure(root: Path, workflow: str) -> RegistryGraph:
    nodes: list[tuple[str, str, Path]] = []
    edges: list[RegistryEdge] = []
    refs: list[tuple[str, Path, str, tuple[str, ...], int]] = []
    recorded_nodes: set[tuple[str, str]] = set()

    def visit(
        kind: str,
        name: str,
        *,
        phase: str,
        ancestry: tuple[str, ...],
        ancestors: tuple[tuple[str, str], ...],
    ) -> None:
        key = (kind, name)
        if key in ancestors:
            raise SafetyError(f"step-registry dependency cycle reaches {kind} {name}")
        path = unique_registry_file(root, name, kind)
        relative_path = path.relative_to(root)
        document_kind, node = _registry_document(
            path.read_text(), context=f"step-registry {relative_path}"
        )
        if document_kind != kind or node.get("as") != name:
            raise SafetyError(
                f"step-registry {relative_path} does not identify exact {kind} {name}"
            )
        if key not in recorded_nodes:
            nodes.append((kind, name, relative_path))
            recorded_nodes.add(key)
        if kind == "ref":
            return
        for (
            child_phase,
            position,
            child_kind,
            child_name,
            companions,
        ) in _node_dependencies(kind, node, context=f"step-registry {relative_path}"):
            effective_phase = child_phase or phase
            child_path = unique_registry_file(root, child_name, child_kind).relative_to(
                root
            )
            locator = f"{kind}:{name}/{effective_phase}[{position}]"
            child_ancestry = ancestry + (locator,)
            edges.append(
                RegistryEdge(
                    parent_kind=kind,
                    parent_name=name,
                    parent_path=str(relative_path),
                    phase=effective_phase,
                    ancestry=child_ancestry,
                    position=position,
                    kind=child_kind,
                    name=child_name,
                    path=str(child_path),
                    companions=json.dumps(
                        companions, sort_keys=True, separators=(",", ":")
                    ),
                )
            )
            if child_kind == "ref":
                refs.append(
                    (
                        child_name,
                        root / child_path,
                        effective_phase,
                        child_ancestry,
                        position,
                    )
                )
            visit(
                child_kind,
                child_name,
                phase=effective_phase,
                ancestry=child_ancestry,
                ancestors=ancestors + (key,),
            )

    visit("workflow", workflow, phase="root", ancestry=(), ancestors=())
    return RegistryGraph(workflow, tuple(nodes), tuple(edges), tuple(refs))


def ordered_registry_refs(
    root: Path, workflow: str
) -> tuple[tuple[str, Path, str, tuple[str, ...], int], ...]:
    return registry_closure(root, workflow).refs


def _file_digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _validate_release_payload(config: Any, version: str) -> str:
    if not isinstance(config, dict):
        raise SafetyError("release config is not a YAML object")
    releases = config.get("releases")
    latest = releases.get("latest") if isinstance(releases, dict) else None
    if not isinstance(latest, dict) or len(latest) != 1:
        raise SafetyError(
            "releases.latest must contain one supported payload definition"
        )
    kind, definition = next(iter(latest.items()))
    if kind not in {"candidate", "release"} or not isinstance(definition, dict):
        raise SafetyError(f"unsupported releases.latest payload definition: {kind}")
    if str(definition.get("version") or "") != version:
        raise SafetyError(
            f"releases.latest payload does not match requested OCP {version}"
        )
    if kind == "candidate" and definition.get("product") != "ocp":
        raise SafetyError("releases.latest candidate is not an OCP payload")
    if kind == "release" and not str(definition.get("channel") or ""):
        raise SafetyError("releases.latest release has no channel")
    return f"{kind}:{definition['version']}"


def _closure_env_values(root: Path, closure: RegistryGraph, key: str) -> list[str]:
    values: list[str] = []
    for kind, _, relative_path in closure.nodes:
        _, node = _registry_document(
            (root / relative_path).read_text(),
            context=f"step-registry {relative_path}",
        )
        env: Any = None
        if kind == "workflow":
            steps = node.get("steps")
            env = steps.get("env") if isinstance(steps, dict) else None
        elif kind == "chain":
            env = node.get("env")
        if isinstance(env, dict) and key in env:
            values.append(str(env[key]))
    for edge in closure.edges:
        companions = json.loads(edge.companions)
        env = companions.get("env") if isinstance(companions, dict) else None
        if isinstance(env, dict) and key in env:
            values.append(str(env[key]))
    return values


def _workflow_env(root: Path, workflow: str) -> dict[str, Any]:
    path = unique_registry_file(root, workflow, "workflow")
    document = strict_yaml_load(path.read_text(), context=f"workflow {workflow}")
    workflow_data = document.get("workflow") if isinstance(document, dict) else None
    steps = workflow_data.get("steps") if isinstance(workflow_data, dict) else None
    env = steps.get("env") if isinstance(steps, dict) else None
    if not isinstance(env, dict):
        raise SafetyError(f"workflow {workflow} has no structured steps.env")
    return env


def _command_for_ref(path: Path, workflow: str) -> Path:
    kind, node = _registry_document(
        path.read_text(), context=f"catalog ref for {workflow}"
    )
    command_name = node.get("commands") if kind == "ref" else None
    if (
        not isinstance(command_name, str)
        or not command_name
        or Path(command_name).name != command_name
    ):
        raise SafetyError(f"catalog ref for {workflow} has an ambiguous command file")
    command_path = path.parent / command_name
    if not command_path.is_file():
        raise SafetyError(f"catalog command file is missing for {workflow}")
    return command_path


def _normalized_template_digest(text: str) -> str:
    """Hash a public script after only line-ending and trailing-space cleanup."""
    normalized = text.replace("\r\n", "\n").replace("\r", "\n")
    normalized = "\n".join(line.rstrip(" \t") for line in normalized.split("\n"))
    normalized = normalized.rstrip("\n") + "\n"
    return hashlib.sha256(normalized.encode()).hexdigest()


def _validate_trusted_template(
    root: Path, ref_name: str, ref_path: Path, workflow: str
) -> tuple[Path, dict[str, str]]:
    template = TRUSTED_SCRIPT_TEMPLATES[ref_name]
    actual_ref_path = str(ref_path.relative_to(root))
    command_path = _command_for_ref(ref_path, workflow)
    actual_command_path = str(command_path.relative_to(root))
    actual_digest = _normalized_template_digest(command_path.read_text())
    if (
        actual_ref_path != template["ref_path"]
        or actual_command_path != template["command_path"]
        or actual_digest != template["digest"]
    ):
        raise SafetyError(
            f"workflow {workflow} has unsupported {template['role']} template shape "
            f"for {ref_name}; review the complete public script and update the "
            "explicit template allowlist before using it"
        )
    return command_path, {
        "identity": template["identity"],
        "role": template["role"],
        "path": template["command_path"],
        "digest": actual_digest,
    }


def verify_latest_catalog(
    root: Path, workflows: Iterable[str]
) -> tuple[
    dict[str, str],
    dict[str, tuple[dict[str, str], ...]],
    dict[str, dict[str, str]],
    dict[str, str],
]:
    """Verify exact vetted producer/consumer scripts and their closure roles."""
    catalog_paths: dict[str, str] = {}
    catalog_templates: dict[str, tuple[dict[str, str], ...]] = {}
    closure_manifests: dict[str, dict[str, str]] = {}
    safety_inputs: dict[str, str] = {}
    trusted_names = set(TRUSTED_SCRIPT_TEMPLATES)
    for workflow in sorted(set(workflows)):
        closure = registry_closure(root, workflow)
        expected_topology = AUDITED_WORKFLOW_TOPOLOGIES.get(workflow)
        if expected_topology is None or closure.topology_digest != expected_topology:
            raise SafetyError(
                f"workflow {workflow} has unsupported phase/order topology; audit the "
                "complete public closure before updating the versioned manifest"
            )
        for _, _, path in closure.nodes:
            safety_inputs[str(path)] = _file_digest(root / path)

        ordered_refs = closure.refs
        ref_names = [name for name, _, _, _, _ in ordered_refs]
        setup_names = [
            name
            for name in ref_names
            if name in SUPPORTED_CATALOG_REFS or "setup-wmco" in name
        ]
        if len(setup_names) != 1 or setup_names[0] not in SUPPORTED_CATALOG_REFS:
            raise SafetyError(
                f"workflow {workflow} must resolve through exactly one supported WMCO "
                "catalog setup path"
            )
        setup_name = setup_names[0]
        required_roles = (
            (CONNECTED_CATALOG_REF, OPERATORHUB_SUBSCRIBE_REF)
            if setup_name == CONNECTED_CATALOG_REF
            else (DISCONNECTED_CATALOG_REF, DISCONNECTED_INSTALL_REF)
        )
        for required in required_roles:
            if ref_names.count(required) != 1:
                raise SafetyError(
                    f"workflow {workflow} must contain exactly one trusted {required} role"
                )
        unexpected_trusted = trusted_names.intersection(ref_names).difference(
            required_roles
        )
        if unexpected_trusted:
            raise SafetyError(
                f"workflow {workflow} has unsupported trusted-role arrangement: "
                + ", ".join(sorted(unexpected_trusted))
            )
        role_refs = {
            name: (phase, ancestry, position)
            for name, _, phase, ancestry, position in ordered_refs
            if name in required_roles
        }
        if any(role_refs[name][0] != "pre" for name in required_roles):
            raise SafetyError(
                f"workflow {workflow} trusted catalog roles must remain in pre/provision"
            )
        positions = [ref_names.index(name) for name in required_roles]
        if positions != sorted(positions):
            raise SafetyError(
                f"workflow {workflow} must run the vetted producer before its consumer"
            )

        template_records: list[dict[str, str]] = []
        command_records: list[str] = []
        for ref_name, ref_path, _, _, _ in ordered_refs:
            command_path = _command_for_ref(ref_path, workflow)
            safety_inputs[str(command_path.relative_to(root))] = _file_digest(
                command_path
            )
            relative_command = str(command_path.relative_to(root))
            actual_command_digest = _normalized_template_digest(
                command_path.read_text()
            )
            expected_command_digest = AUDITED_COMMAND_DIGESTS.get(relative_command)
            if expected_command_digest != actual_command_digest:
                raise SafetyError(
                    f"workflow {workflow} has unsupported executable closure command "
                    f"{relative_command}; review the complete public script and update "
                    "the versioned manifest before using this template shape"
                )
            command_records.append(f"{relative_command}:{actual_command_digest}")
            if ref_name in required_roles:
                validated_path, record = _validate_trusted_template(
                    root, ref_name, ref_path, workflow
                )
                if validated_path != command_path:
                    raise SafetyError(
                        f"workflow {workflow} resolved an inconsistent trusted template"
                    )
                template_records.append(record)
                continue
            if ref_name in trusted_names:
                raise SafetyError(
                    f"workflow {workflow} has unsupported trusted role {ref_name}"
                )

        if setup_name == CONNECTED_CATALOG_REF:
            workflow_env = _workflow_env(root, workflow)
            for key, expected in (
                ("SUB_CHANNEL", "stable"),
                ("SUB_PACKAGE", "windows-machine-config-operator"),
                ("SUB_SOURCE", "wmco"),
            ):
                values = _closure_env_values(root, closure, key)
                if values != [expected] or workflow_env.get(key) != expected:
                    raise SafetyError(
                        f"workflow {workflow} has an ambiguous or overridden {key}"
                    )

        catalog_paths[workflow] = setup_name
        catalog_templates[workflow] = tuple(template_records)
        command_inventory_digest = hashlib.sha256(
            "\n".join(sorted(command_records)).encode()
        ).hexdigest()
        closure_manifests[workflow] = {
            "identity": CLOSURE_MANIFEST_IDENTITY,
            "schema": str(CLOSURE_MANIFEST_SCHEMA),
            "topology_digest": closure.topology_digest,
            "command_inventory_digest": command_inventory_digest,
        }
        safety_inputs[f"manifest:{workflow}:identity"] = hashlib.sha256(
            CLOSURE_MANIFEST_IDENTITY.encode()
        ).hexdigest()
        safety_inputs[f"manifest:{workflow}:schema"] = hashlib.sha256(
            str(CLOSURE_MANIFEST_SCHEMA).encode()
        ).hexdigest()
        safety_inputs[f"manifest:{workflow}:topology"] = closure.topology_digest
        safety_inputs[f"manifest:{workflow}:commands"] = command_inventory_digest
    return catalog_paths, catalog_templates, closure_manifests, safety_inputs


def build_plan(
    root: Path, version: str, stream: str, *, base_sha: str = "fixture"
) -> JobPlan:
    version = validate_ocp_version(version)
    stream = validate_stream(stream)
    root = root.resolve()
    config_path = confined_path(
        root,
        Path(CONFIG_RELATIVE.format(version=version)),
        context="release config",
    )
    generated_path = confined_path(
        root,
        Path(JOBS_RELATIVE.format(version=version)),
        context="generated jobs",
    )
    if not config_path.is_file():
        raise SafetyError(
            f"release config does not exist for OCP {version}: {config_path}"
        )
    config_text = config_path.read_text()
    config = strict_yaml_load(config_text, context="release config")
    payload_definition = _validate_release_payload(config, version)
    discovered = discover_wmco_jobs(config_text)
    selected = select_jobs(discovered, stream)
    blocks = top_level_test_blocks(config_text)
    workflows = {name: workflow_for_job(blocks[name], name) for name in selected}
    catalog_paths, catalog_templates, closure_manifests, safety_inputs = (
        verify_latest_catalog(root, workflows.values())
    )
    safety_inputs[str(config_path.relative_to(root))] = _file_digest(config_path)
    renamed = {name: renamed_job(name, stream) for name in selected}
    return JobPlan(
        version=version,
        stream=stream,
        config_path=config_path,
        generated_path=generated_path,
        discovered=discovered,
        selected=selected,
        renamed=renamed,
        workflows=workflows,
        base_sha=base_sha,
        payload_definition=payload_definition,
        catalog_paths=catalog_paths,
        catalog_templates=catalog_templates,
        closure_manifests=closure_manifests,
        safety_inputs=safety_inputs,
    )


def apply_config_renames(config_text: str, plan: JobPlan) -> str:
    original_names = [match.group("name") for match in AS_LINE_RE.finditer(config_text)]
    updated = config_text
    for old_name in plan.selected:
        new_name = plan.renamed[old_name]
        pattern = re.compile(rf"(?m)^- as: {re.escape(old_name)}$")
        updated, count = pattern.subn(f"- as: {new_name}", updated)
        if count != 1:
            raise SafetyError(
                f"expected one config rename for {old_name}, found {count}"
            )
    updated_names = [match.group("name") for match in AS_LINE_RE.finditer(updated)]
    expected_names = [plan.renamed.get(name, name) for name in original_names]
    if updated_names != expected_names:
        raise SafetyError("config as values changed outside the selected rename set")
    return updated


def validate_config_renames(before: str, after: str, plan: JobPlan) -> None:
    expected = apply_config_renames(before, plan)
    if after != expected:
        raise SafetyError("release config differs from the exact planned renames")
    for old_name, new_name in plan.renamed.items():
        if len(re.findall(rf"(?m)^- as: {re.escape(new_name)}$", after)) != 1:
            raise SafetyError(
                f"renamed config value is missing or duplicated: {new_name}"
            )
        if re.search(rf"(?m)^- as: {re.escape(old_name)}$", after):
            raise SafetyError(f"old config value remains after rename: {old_name}")


def generated_blocks(generated_text: str) -> tuple[str, ...]:
    return tuple(block for block in re.split(r"(?m)(?=^- )", generated_text) if block)


def validate_generated_jobs(generated_text: str, plan: JobPlan) -> None:
    blocks = generated_blocks(generated_text)
    for old_name, new_name in plan.renamed.items():
        expected_name = (
            "periodic-ci-openshift-openshift-tests-private-release-"
            f"{plan.version}-amd64-nightly-{new_name}"
        )
        matching = [
            block
            for block in blocks
            if re.search(rf"(?m)^\s+name: {re.escape(expected_name)}$", block)
        ]
        if len(matching) != 1:
            raise SafetyError(
                f"expected one generated periodic for {new_name}, found {len(matching)}"
            )
        target_matches = re.findall(
            rf"(?m)^\s+- --target={re.escape(new_name)}$", matching[0]
        )
        if len(target_matches) != 1:
            raise SafetyError(
                f"generated periodic {expected_name} does not target {new_name} exactly once"
            )
        if 'pj-rehearse.openshift.io/can-be-rehearsed: "true"' not in matching[0]:
            raise SafetyError(f"generated periodic is not rehearsable: {expected_name}")
        old_generated_name = expected_name[: -len(new_name)] + old_name
        if old_generated_name in generated_text:
            raise SafetyError(f"old generated periodic remains: {old_generated_name}")
        if re.search(rf"(?m)^\s+- --target={re.escape(old_name)}$", generated_text):
            raise SafetyError(f"old generated target remains: {old_name}")


def validate_generated_diff(before: str, after: str, plan: JobPlan) -> None:
    expected = before
    for old_name, new_name in plan.renamed.items():
        old_prow = (
            "periodic-ci-openshift-openshift-tests-private-release-"
            f"{plan.version}-amd64-nightly-{old_name}"
        )
        new_prow = (
            "periodic-ci-openshift-openshift-tests-private-release-"
            f"{plan.version}-amd64-nightly-{new_name}"
        )
        expected, name_count = re.subn(
            rf"(?m)^(\s+name: ){re.escape(old_prow)}$",
            rf"\g<1>{new_prow}",
            expected,
        )
        expected, target_count = re.subn(
            rf"(?m)^(\s+- --target=){re.escape(old_name)}$",
            rf"\g<1>{new_name}",
            expected,
        )
        if name_count != 1 or target_count != 1:
            raise SafetyError(
                f"generated baseline does not uniquely contain {old_name}"
            )
    if after != expected:
        raise SafetyError(
            "generated periodics contain changes beyond planned name/target substitutions"
        )
    validate_generated_jobs(after, plan)


def changed_paths_from_porcelain(status_text: str) -> tuple[set[str], bool]:
    paths: set[str] = set()
    has_untracked = False
    for line in status_text.splitlines():
        if not line:
            continue
        if line.startswith("? "):
            has_untracked = True
            paths.add(line[2:])
        elif line.startswith(("1 ", "2 ")):
            paths.add(line.split(" ", 8)[-1].split("\t")[-1])
        else:
            raise SafetyError(f"unsupported git status entry: {line}")
    return paths, has_untracked


def parse_notifier_jobs(comment_body: str) -> tuple[str, ...]:
    lines = comment_body.splitlines()
    nonempty = [index for index, line in enumerate(lines) if line.strip()]
    if not nonempty or lines[nonempty[0]].strip() != "[REHEARSALNOTIFIER]":
        raise SafetyError("comment is not an authoritative REHEARSALNOTIFIER comment")
    marker_count = sum(line.strip() == "[REHEARSALNOTIFIER]" for line in lines)
    if marker_count != 1:
        raise SafetyError("REHEARSALNOTIFIER marker is duplicated or embedded")
    header_indices = [
        index
        for index, line in enumerate(lines)
        if line.strip() == "Test name | Repo | Type | Reason"
    ]
    if len(header_indices) != 1:
        raise SafetyError("REHEARSALNOTIFIER has no unique expected table header")
    header = header_indices[0]
    if header + 1 >= len(lines) or not re.fullmatch(
        r"\s*---\s*\|\s*---\s*\|\s*---\s*\|\s*---\s*", lines[header + 1]
    ):
        raise SafetyError("REHEARSALNOTIFIER table separator is malformed")
    jobs: list[str] = []
    for line in lines[header + 2 :]:
        if not line.strip() or "|" not in line:
            if jobs:
                break
            continue
        cells = [cell.strip() for cell in line.strip().strip("|").split("|")]
        if len(cells) != 4:
            raise SafetyError("REHEARSALNOTIFIER job table is malformed")
        first_cell = cells[0].strip("`")
        if first_cell.startswith("periodic-ci-"):
            jobs.append(first_cell)
        elif jobs:
            raise SafetyError("REHEARSALNOTIFIER job table contains a malformed row")
    if len(jobs) != len(set(jobs)):
        raise SafetyError("REHEARSALNOTIFIER contains duplicate job rows")
    return tuple(jobs)


def validate_notifier_coverage(
    comment_body: str, expected: Iterable[str]
) -> tuple[str, ...]:
    notified = parse_notifier_jobs(comment_body)
    expected_set = set(expected)
    notified_set = set(notified)
    if notified_set != expected_set:
        missing = sorted(expected_set - notified_set)
        unexpected = sorted(notified_set - expected_set)
        raise SafetyError(
            "REHEARSALNOTIFIER coverage is incomplete or unexpected; "
            f"missing={missing}, unexpected={unexpected}"
        )
    return notified


def authoritative_notifier_comments(
    comments: Iterable[dict[str, Any]],
) -> tuple[dict[str, Any], ...]:
    authoritative: list[dict[str, Any]] = []
    for comment in comments:
        body = str(comment.get("body") or "")
        if "[REHEARSALNOTIFIER]" not in body:
            continue
        user = comment.get("user")
        if not isinstance(user, dict):
            continue
        if user.get("login") != NOTIFIER_LOGIN or user.get("type") != "Bot":
            continue
        app = comment.get("performed_via_github_app") or comment.get("app")
        if not isinstance(app, dict) or app.get("slug") != NOTIFIER_APP_SLUG:
            continue
        try:
            parse_notifier_jobs(body)
        except SafetyError:
            continue
        authoritative.append(comment)
    return tuple(authoritative)


def notifier_identity(comment: dict[str, Any]) -> tuple[str, str | None]:
    user = comment.get("user")
    app = comment.get("performed_via_github_app") or comment.get("app")
    login = str(user.get("login") or "") if isinstance(user, dict) else ""
    slug = str(app.get("slug") or "") if isinstance(app, dict) else None
    return login, slug


NON_TRIGGER_REHEARSE_ACTIONS = {
    "abort",
    "ack",
    "list",
    "network-access-allowed",
    "reject",
    "skip",
}


def trigger_rehearsal_commands(body: str) -> tuple[str, ...]:
    commands: list[str] = []
    for line in body.splitlines():
        stripped = line.strip()
        if not stripped.startswith("/pj-rehearse"):
            continue
        tokens = stripped.split()
        if tokens[0] != "/pj-rehearse":
            continue
        if len(tokens) == 1 or tokens[1] not in NON_TRIGGER_REHEARSE_ACTIONS:
            commands.append(stripped)
    return tuple(commands)


def validate_recorded_notifier(
    comments: Iterable[dict[str, Any]], state: dict[str, Any]
) -> None:
    comment_list = list(comments)
    notifier = [
        comment
        for comment in authoritative_notifier_comments(comment_list)
        if comment.get("id") == state.get("notifier_comment_id")
    ]
    if len(notifier) != 1:
        raise SafetyError(
            "recorded authoritative notifier comment is missing or changed"
        )
    validate_notifier_coverage(
        str(notifier[0].get("body") or ""), state["expected_prow_jobs"]
    )
    login, app_slug = notifier_identity(notifier[0])
    if login != state.get("notifier_author") or app_slug != state.get(
        "notifier_app_slug"
    ):
        raise SafetyError("recorded notifier publisher identity changed")


def validate_request_comments(
    comments: Iterable[dict[str, Any]], state: dict[str, Any]
) -> None:
    comment_list = list(comments)
    validate_recorded_notifier(comment_list, state)
    requests = [
        comment
        for comment in comment_list
        if comment.get("id") == state.get("request_comment_id")
        and str(comment.get("body") or "").strip() == state.get("rehearsal_command")
        and str(comment.get("created_at") or "") == state.get("request_created_at")
        and isinstance(comment.get("user"), dict)
        and comment["user"].get("login") == state.get("request_author")
        and str(comment.get("html_url") or "") == state.get("request_url")
    ]
    if len(requests) != 1:
        raise SafetyError("recorded rehearsal request comment is missing or changed")
    other_triggers = [
        comment
        for comment in comment_list
        if comment.get("id") != state.get("request_comment_id")
        and trigger_rehearsal_commands(str(comment.get("body") or ""))
    ]
    if other_triggers:
        raise SafetyError(
            "an additional rehearsal trigger exists after the pinned request"
        )


def rehearsal_command(job_count: int) -> str:
    if job_count < 1:
        raise SafetyError("cannot request rehearsals for an empty job set")
    if job_count > MAX_REHEARSALS:
        raise SafetyError(
            f"{job_count} jobs exceed the authoritative /pj-rehearse max limit of {MAX_REHEARSALS}"
        )
    return "/pj-rehearse max" if job_count > 5 else "/pj-rehearse"


def parse_timestamp(value: str | None) -> dt.datetime | None:
    if not value:
        return None
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed if parsed.tzinfo is not None else None


def normalize_commit_statuses(
    statuses: Iterable[dict[str, Any]], head_sha: str
) -> list[dict[str, Any]]:
    normalized: list[dict[str, Any]] = []
    for status in statuses:
        state_value = str(status.get("state") or "")
        normalized.append(
            {
                "name": status.get("context"),
                "status": "pending" if state_value == "pending" else "completed",
                "conclusion": state_value,
                "head_sha": head_sha,
                "created_at": status.get("created_at"),
                "updated_at": status.get("updated_at"),
                "started_at": status.get("created_at"),
                "details_url": status.get("target_url"),
                "publisher": status.get("creator"),
                "app": status.get("performed_via_github_app") or status.get("app"),
                "source": "commit_status",
            }
        )
    return normalized


def _publisher_is_authoritative(run: dict[str, Any]) -> bool:
    publisher = run.get("publisher") or run.get("creator")
    app = run.get("app")
    login = publisher.get("login") if isinstance(publisher, dict) else None
    slug = app.get("slug") if isinstance(app, dict) else None
    if login is not None and login != AUTHORITATIVE_PROW_ACCOUNT:
        return False
    if slug is not None and slug != PROW_PUBLISHER_APP_SLUG:
        return False
    return login == AUTHORITATIVE_PROW_ACCOUNT or slug == PROW_PUBLISHER_APP_SLUG


def _prow_build_identity(url: str, pr_number: int, job: str) -> str | None:
    parsed = urlparse(url)
    if (
        parsed.scheme != "https"
        or parsed.hostname not in PROW_HOSTS
        or parsed.query
        or parsed.fragment
    ):
        return None
    prefix = rf"/(?:view/gs|gcs)/[^/]+/pr-logs/pull/openshift_release/{pr_number}/"
    suffix = rf"rehearse-{pr_number}-{re.escape(job)}/(?P<build>[0-9]+)/*"
    match = re.fullmatch(prefix + suffix, parsed.path)
    if not match:
        return None
    return f"openshift_release:{pr_number}:{job}:{match.group('build')}"


def _run_timestamp(run: dict[str, Any]) -> dt.datetime | None:
    for key in ("updated_at", "completed_at", "started_at", "created_at"):
        parsed = parse_timestamp(run.get(key))
        if parsed is not None:
            return parsed
    return None


def _latest_transition(
    indexed: Iterable[tuple[int, dict[str, Any]]],
) -> dict[str, Any] | None:
    candidates = list(indexed)
    latest_time = max(
        _run_timestamp(run) or dt.datetime.min.replace(tzinfo=dt.timezone.utc)
        for _, run in candidates
    )
    latest = [
        run
        for _, run in candidates
        if (_run_timestamp(run) or dt.datetime.min.replace(tzinfo=dt.timezone.utc))
        == latest_time
    ]
    semantics = {
        (
            str(run.get("status") or ""),
            str(run.get("conclusion") or ""),
            str(run.get("details_url") or ""),
        )
        for run in latest
    }
    if len(semantics) != 1:
        return None
    return latest[0]


@dataclasses.dataclass(frozen=True)
class JobEvidence:
    job: str
    state: str
    url: str | None
    detail: str


@dataclasses.dataclass(frozen=True)
class MonitoringDecision:
    outcome: str
    evidence: tuple[JobEvidence, ...]

    @property
    def may_close(self) -> bool:
        return self.outcome == "success" and all(
            item.state == "success" for item in self.evidence
        )


def evaluate_check_runs(
    check_runs: Iterable[dict[str, Any]],
    expected_jobs: Iterable[str],
    *,
    head_sha: str,
    request_created_at: str,
    pr_number: int,
) -> MonitoringDecision:
    request_time = parse_timestamp(request_created_at)
    if request_time is None:
        raise SafetyError("rehearsal request timestamp is missing or invalid")
    runs = list(check_runs)
    evidence: list[JobEvidence] = []
    for job in expected_jobs:
        check_name = f"ci/rehearse/{job}"
        candidates: dict[str, list[tuple[int, dict[str, Any]]]] = {}
        null_url_candidates: list[tuple[int, dict[str, Any]]] = []
        anomalous_current: list[str] = []
        stale_count = 0
        for index, run in enumerate(runs):
            if run.get("name") != check_name:
                continue
            if run.get("head_sha") != head_sha:
                stale_count += 1
                continue
            details_url = str(run.get("details_url") or "")
            created = parse_timestamp(run.get("started_at") or run.get("created_at"))
            if created is None:
                if not details_url:
                    null_url_candidates.append((index, run))
                else:
                    stale_count += 1
                continue
            if created < request_time:
                stale_count += 1
                continue
            if not _publisher_is_authoritative(run):
                anomalous_current.append("foreign publisher")
                continue
            if not details_url:
                null_url_candidates.append((index, run))
                continue
            identity = _prow_build_identity(details_url, pr_number, job)
            if identity is None:
                anomalous_current.append("invalid Prow URL")
                continue
            candidates.setdefault(identity, []).append((index, run))
        if anomalous_current:
            evidence.append(
                JobEvidence(
                    job,
                    "ambiguous",
                    None,
                    "current request evidence has "
                    + " and ".join(sorted(set(anomalous_current))),
                )
            )
            continue
        if len(candidates) == 0 and not null_url_candidates:
            detail = "missing current request evidence"
            if stale_count:
                detail += f" ({stale_count} stale or unpinned run(s) ignored)"
            evidence.append(JobEvidence(job, "missing", None, detail))
            continue
        if len(candidates) > 1:
            evidence.append(
                JobEvidence(job, "ambiguous", None, "multiple current request runs")
            )
            continue
        if candidates:
            concrete_transitions = next(iter(candidates.values()))
            concrete_times = [_run_timestamp(item[1]) for item in concrete_transitions]
            unorderable_null = any(
                _run_timestamp(item[1]) is None for item in null_url_candidates
            )
            earliest_concrete = min(
                timestamp for timestamp in concrete_times if timestamp is not None
            )
            newer_null = [
                item
                for item in null_url_candidates
                if _run_timestamp(item[1]) is not None
                and _run_timestamp(item[1]) >= earliest_concrete
            ]
            if unorderable_null:
                evidence.append(
                    JobEvidence(
                        job,
                        "ambiguous",
                        None,
                        "null-URL transition cannot be ordered before the pinned build",
                    )
                )
                continue
            if newer_null:
                run = _latest_transition(newer_null)
            else:
                run = _latest_transition(concrete_transitions)
        else:
            run = _latest_transition(null_url_candidates)
        if run is None:
            evidence.append(
                JobEvidence(
                    job,
                    "ambiguous",
                    None,
                    "conflicting maximum-timestamp transitions for one logical build",
                )
            )
            continue
        status = str(run.get("status") or "")
        conclusion = run.get("conclusion")
        url = str(run.get("details_url") or "") or None
        if status in {"queued", "in_progress", "requested", "waiting", "pending"}:
            evidence.append(JobEvidence(job, "pending", url, status))
        elif status != "completed":
            evidence.append(
                JobEvidence(job, "unrecognized", url, status or "empty status")
            )
        elif conclusion == "success":
            if url is None:
                evidence.append(
                    JobEvidence(job, "error", None, "success has no pinned Prow build")
                )
            else:
                evidence.append(
                    JobEvidence(job, "success", url, "completed successfully")
                )
        elif conclusion in {"cancelled", "timed_out", "stale"}:
            evidence.append(JobEvidence(job, "aborted", url, str(conclusion)))
        elif conclusion in {"failure", "action_required"}:
            evidence.append(JobEvidence(job, "failure", url, str(conclusion)))
        elif conclusion in {None, "", "error", "neutral", "skipped"}:
            evidence.append(JobEvidence(job, "error", url, str(conclusion)))
        else:
            evidence.append(JobEvidence(job, "unrecognized", url, str(conclusion)))
    states = {item.state for item in evidence}
    if states == {"success"}:
        outcome = "success"
    elif states <= {"success", "pending", "missing"}:
        outcome = "waiting"
    else:
        outcome = "blocked"
    return MonitoringDecision(outcome, tuple(evidence))


ENGINE_GLOBAL_OPTIONS: dict[str, dict[str, int]] = {
    "podman": {
        "--connection": 1,
        "--events-backend": 1,
        "--identity": 1,
        "--log-level": 1,
        "--remote": 0,
        "--root": 1,
        "--runroot": 1,
        "--runtime": 1,
        "--storage-driver": 1,
        "--tmpdir": 1,
        "--transient-store": 0,
        "--url": 1,
        "-c": 1,
    },
    "docker": {
        "--config": 1,
        "--context": 1,
        "--debug": 0,
        "--host": 1,
        "--log-level": 1,
        "--tls": 0,
        "--tlscacert": 1,
        "--tlscert": 1,
        "--tlskey": 1,
        "--tlsverify": 0,
        "-D": 0,
        "-H": 1,
    },
}
PULL_OPTIONS: dict[str, dict[str, int]] = {
    "podman": {
        "--all-tags": 0,
        "--arch": 1,
        "--authfile": 1,
        "--cert-dir": 1,
        "--creds": 1,
        "--decryption-key": 1,
        "--os": 1,
        "--platform": 1,
        "--policy": 1,
        "--quiet": 0,
        "--retry": 1,
        "--retry-delay": 1,
        "--tls-verify": 1,
        "--variant": 1,
        "-a": 0,
        "-q": 0,
    },
    "docker": {
        "--all-tags": 0,
        "--disable-content-trust": 1,
        "--platform": 1,
        "--quiet": 0,
        "-a": 0,
        "-q": 0,
    },
}
IMAGE_REFERENCE_RE = re.compile(
    r"^[A-Za-z0-9][A-Za-z0-9._:-]*(?:/[A-Za-z0-9][A-Za-z0-9._-]*)+"
    r"(?::[A-Za-z0-9_][A-Za-z0-9_.-]*)?(?:@sha256:[0-9a-f]{64})?$"
)


def _valid_options(tokens: Sequence[str], arities: dict[str, int]) -> bool:
    index = 0
    while index < len(tokens):
        token = tokens[index]
        option, separator, inline_value = token.partition("=")
        arity = arities.get(option)
        if arity is None:
            return False
        if separator:
            if arity != 1 or not inline_value:
                return False
            index += 1
        elif arity == 1:
            if index + 1 >= len(tokens) or tokens[index + 1].startswith("-"):
                return False
            index += 2
        else:
            index += 1
    return True


def pull_image_from_dry_run(line: str, container_engine: str) -> str | None:
    try:
        tokens = shlex.split(line)
    except ValueError as exc:
        raise SafetyError("make update dry-run output is malformed") from exc
    if tokens[:2] == ["false", "||"]:
        tokens = tokens[2:]
    if not tokens or tokens[0] != container_engine:
        return None
    pull_indices = [index for index, token in enumerate(tokens) if token == "pull"]
    if len(pull_indices) != 1:
        return None
    pull_index = pull_indices[0]
    if not _valid_options(
        tokens[1:pull_index], ENGINE_GLOBAL_OPTIONS[container_engine]
    ):
        return None
    if len(tokens) <= pull_index + 1:
        return None
    image = tokens[-1]
    if not IMAGE_REFERENCE_RE.fullmatch(image):
        return None
    if not _valid_options(tokens[pull_index + 1 : -1], PULL_OPTIONS[container_engine]):
        return None
    return image


def json_object(text: str, context: str) -> dict[str, Any]:
    try:
        value = json.loads(text)
    except json.JSONDecodeError as exc:
        raise SafetyError(f"{context} returned invalid JSON") from exc
    if not isinstance(value, dict):
        raise SafetyError(f"{context} did not return a JSON object")
    return value


def json_array(text: str, context: str) -> list[dict[str, Any]]:
    try:
        value = json.loads(text)
    except json.JSONDecodeError as exc:
        raise SafetyError(f"{context} returned invalid JSON") from exc
    if not isinstance(value, list) or not all(isinstance(item, dict) for item in value):
        raise SafetyError(f"{context} did not return a JSON object array")
    return value


def json_paginated_array(text: str, context: str) -> list[dict[str, Any]]:
    try:
        value = json.loads(text)
    except json.JSONDecodeError as exc:
        raise SafetyError(f"{context} returned invalid JSON") from exc
    if not isinstance(value, list):
        raise SafetyError(f"{context} did not return a paginated JSON array")
    if all(isinstance(item, dict) for item in value):
        return value
    if all(isinstance(page, list) for page in value):
        flattened = [item for page in value for item in page]
        if all(isinstance(item, dict) for item in flattened):
            return flattened
    raise SafetyError(f"{context} returned malformed paginated JSON")


def atomic_write_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w") as stream:
            json.dump(value, stream, indent=2, sort_keys=True)
            stream.write("\n")
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def load_state(path: Path) -> dict[str, Any]:
    if not path.is_file():
        raise SafetyError(f"resume state does not exist: {path}")
    try:
        state = json.loads(path.read_text())
    except (json.JSONDecodeError, OSError) as exc:
        raise SafetyError(f"resume state cannot be read safely: {path}") from exc
    if not isinstance(state, dict) or state.get("schema") != STATE_SCHEMA:
        raise SafetyError("resume state schema is missing or unsupported")
    return state


def redact_remote(remote_url: str) -> str:
    if not remote_url or remote_url != remote_url.strip():
        return "unrecognized Git remote"

    scp_match = re.fullmatch(
        r"git@github\.com:([^/]+)/([^/]+)",
        remote_url,
    )
    if scp_match:
        return _validated_remote_repository(*scp_match.groups())

    try:
        parsed = urlparse(remote_url)
        port = parsed.port
    except ValueError:
        return "unrecognized Git remote"
    if parsed.query or parsed.fragment or parsed.params:
        return "unrecognized Git remote"

    path_parts = parsed.path.split("/")
    if (
        parsed.scheme == "https"
        and parsed.hostname == "github.com"
        and parsed.username is None
        and parsed.password is None
        and port is None
        and len(path_parts) == 3
        and path_parts[0] == ""
    ):
        return _validated_remote_repository(path_parts[1], path_parts[2])
    if (
        parsed.scheme == "ssh"
        and parsed.hostname == "github.com"
        and parsed.username == "git"
        and parsed.password is None
        and port in {None, 22}
        and len(path_parts) == 3
        and path_parts[0] == ""
    ):
        return _validated_remote_repository(path_parts[1], path_parts[2])
    if (
        parsed.scheme == "http"
        and parsed.hostname == "localhost"
        and parsed.username is None
        and parsed.password is None
        and port is not None
        and len(path_parts) == 4
        and path_parts[:2] == ["", "github.com"]
    ):
        return _validated_remote_repository(path_parts[2], path_parts[3])
    return "unrecognized Git remote"


def _validated_remote_repository(owner: str, repository: str) -> str:
    repository = repository.removesuffix(".git")
    owner_pattern = r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?"
    repository_pattern = r"[A-Za-z0-9_.-]+"
    if not re.fullmatch(owner_pattern, owner) or not re.fullmatch(
        repository_pattern, repository
    ):
        return "unrecognized Git remote"
    if repository in {".", ".."}:
        return "unrecognized Git remote"
    return f"{owner}/{repository}"


def snapshot_fork_repository(snapshot: dict[str, Any]) -> str:
    repository = snapshot.get("headRepository")
    owner = snapshot.get("headRepositoryOwner")
    full_name = (
        str(repository.get("nameWithOwner") or "")
        if isinstance(repository, dict)
        else ""
    )
    owner_login = str(owner.get("login") or "") if isinstance(owner, dict) else ""
    repo_name = (
        str(repository.get("name") or "") if isinstance(repository, dict) else ""
    )
    composed = f"{owner_login}/{repo_name}" if owner_login and repo_name else ""
    if full_name and composed and full_name != composed:
        raise SafetyError("PR head repository fields disagree")
    result = full_name or composed
    if not result or "/" not in result:
        raise SafetyError("PR snapshot has no exact head repository identity")
    return result


class Lifecycle:
    """Fail-closed live lifecycle with persistent state and no merge operation."""

    def __init__(
        self,
        release_repo: Path,
        runner: Runner,
        *,
        poll_seconds: int = 30,
        notifier_timeout: int = 600,
        monitor_timeout: int = 21600,
        container_engine: str = "podman",
    ):
        self.release_repo = release_repo.resolve()
        self.runner = runner
        self.poll_seconds = poll_seconds
        self.notifier_timeout = notifier_timeout
        self.monitor_timeout = monitor_timeout
        self.container_engine = container_engine

    def command(
        self,
        args: Sequence[str],
        *,
        cwd: Path | None = None,
        check: bool = True,
    ) -> CommandResult:
        return self.runner.run(args, cwd=cwd or self.release_repo, check=check)

    def git(
        self, args: Sequence[str], *, cwd: Path | None = None, check: bool = True
    ) -> CommandResult:
        return self.command(["git", *args], cwd=cwd, check=check)

    def gh(
        self, args: Sequence[str], *, cwd: Path | None = None, check: bool = True
    ) -> CommandResult:
        return self.command(["gh", *args], cwd=cwd, check=check)

    def origin_repository(self) -> str:
        fork_url = self.git(["remote", "get-url", FORK_REMOTE]).stdout.strip()
        fork_repo = redact_remote(fork_url)
        if fork_repo == "unrecognized Git remote" or fork_repo == UPSTREAM_REPOSITORY:
            raise SafetyError(
                "origin must be a recognized writable fork of openshift/release"
            )
        info = json_object(
            self.gh(
                [
                    "repo",
                    "view",
                    fork_repo,
                    "--json",
                    "nameWithOwner,parent",
                ]
            ).stdout,
            "fork repository lookup",
        )
        parent = info.get("parent")
        parent_name = parent.get("nameWithOwner") if isinstance(parent, dict) else ""
        if info.get("nameWithOwner") != fork_repo or parent_name != UPSTREAM_REPOSITORY:
            raise SafetyError("origin is not an openshift/release fork")
        return fork_repo

    def authenticated_login(self) -> str:
        info = json_object(
            self.gh(["api", "user"]).stdout, "authenticated GitHub user lookup"
        )
        login = str(info.get("login") or "")
        if not login:
            raise SafetyError("authenticated GitHub user has no login")
        return login

    def preflight_runtime(self) -> None:
        if self.container_engine not in {"podman", "docker"}:
            raise SafetyError("container engine must be podman or docker")
        self.command([self.container_engine, "info"])
        self.command([sys.executable, "-c", "import yaml"])
        self.command(["make", "--version"])
        dry_run = self.command(
            ["make", "-n", "update", f"CONTAINER_ENGINE={self.container_engine}"]
        ).stdout
        pull_images: list[str] = []
        for line in dry_run.splitlines():
            image = pull_image_from_dry_run(line, self.container_engine)
            if image is not None:
                pull_images.append(image)
                continue
            try:
                tokens = shlex.split(line)
            except ValueError as exc:
                raise SafetyError("make update dry-run output is malformed") from exc
            if self.container_engine in tokens and "pull" in tokens:
                raise SafetyError(
                    "make update dry-run contains an unsupported container pull recipe"
                )
        if not pull_images:
            raise SafetyError("make update preflight found no generator image pulls")
        for image in sorted(set(pull_images)):
            self.command([self.container_engine, "pull", "--quiet", image])
        free_bytes = shutil.disk_usage(self.release_repo).free
        if free_bytes < 5 * 1024**3:
            raise SafetyError(
                "less than 5 GiB is free for make update and its worktree"
            )

    def preflight_repository(self, *, prepare_runtime: bool = True) -> tuple[str, str]:
        inside = self.git(["rev-parse", "--is-inside-work-tree"]).stdout.strip()
        if inside != "true":
            raise SafetyError("release repository path is not a Git worktree")
        upstream_url = self.git(["remote", "get-url", UPSTREAM_REMOTE]).stdout.strip()
        if redact_remote(upstream_url) != UPSTREAM_REPOSITORY:
            raise SafetyError("upstream remote is not openshift/release")
        self.command(["gh", "auth", "status"])
        fork_repo = self.origin_repository()
        actor = self.authenticated_login()
        if prepare_runtime:
            self.preflight_runtime()
        return fork_repo, actor

    def state_path(self, version: str, stream: str, run_id: str) -> Path:
        version = validate_ocp_version(version)
        stream = validate_stream(stream)
        run_id = validate_run_id(run_id)
        common_dir_raw = self.git(["rev-parse", "--git-common-dir"]).stdout.strip()
        common_dir = Path(common_dir_raw)
        if not common_dir.is_absolute():
            common_dir = (self.release_repo / common_dir).resolve()
        key = f"ocp-{version.replace('.', '-')}-{stream}stream-{run_id}"
        return confined_path(
            common_dir,
            Path("wmco-qe-trigger") / f"{key}.json",
            context="lifecycle state",
        )

    def branch_name(self, version: str, stream: str, run_id: str) -> str:
        version = validate_ocp_version(version)
        stream = validate_stream(stream)
        run_id = validate_run_id(run_id)
        return f"wmco-qe-ocp-{version.replace('.', '-')}-{stream}stream-{run_id}"

    def worktree_path(self, version: str, stream: str, run_id: str) -> Path:
        version = validate_ocp_version(version)
        stream = validate_stream(stream)
        run_id = validate_run_id(run_id)
        key = f"ocp-{version.replace('.', '-')}-{stream}stream-{run_id}"
        return confined_path(
            self.release_repo.parent,
            Path(".wmco-qe-worktrees") / key,
            context="lifecycle worktree",
        )

    def save(self, path: Path, state: dict[str, Any], **changes: Any) -> dict[str, Any]:
        updated = dict(state)
        updated.update(changes)
        updated["updated_at"] = dt.datetime.now(dt.timezone.utc).isoformat()
        atomic_write_json(path, updated)
        return updated

    def assert_no_duplicate(
        self, branch: str, state_path: Path, fork_repository: str
    ) -> None:
        if state_path.exists():
            raise SafetyError(f"existing lifecycle state requires resume: {state_path}")
        local = self.git(["show-ref", "--verify", f"refs/heads/{branch}"], check=False)
        if local.returncode == 0:
            raise SafetyError(
                f"local branch already exists without lifecycle state: {branch}"
            )
        if local.returncode not in {0, 1}:
            raise CommandError(
                ["git", "show-ref", "--verify", f"refs/heads/{branch}"],
                local.returncode,
                local.stderr,
            )
        remote = self.git(
            ["ls-remote", "--exit-code", FORK_REMOTE, f"refs/heads/{branch}"],
            check=False,
        )
        if remote.returncode == 0:
            raise SafetyError(
                f"remote branch already exists without lifecycle state: {branch}"
            )
        if remote.returncode != 2:
            raise CommandError(
                ["git", "ls-remote", FORK_REMOTE, f"refs/heads/{branch}"],
                remote.returncode,
                remote.stderr,
            )
        pulls = json_array(
            self.gh(
                [
                    "pr",
                    "list",
                    "--repo",
                    UPSTREAM_REPOSITORY,
                    "--state",
                    "open",
                    "--head",
                    branch,
                    "--json",
                    (
                        "number,url,headRefName,headRefOid,isDraft,"
                        "headRepository,headRepositoryOwner"
                    ),
                ]
            ).stdout,
            "duplicate PR lookup",
        )
        if [pr for pr in pulls if snapshot_fork_repository(pr) == fork_repository]:
            raise SafetyError(f"an open temporary PR already uses branch {branch}")

    def plan(
        self,
        version: str,
        stream: str,
        *,
        root: Path | None = None,
        base_sha: str | None = None,
    ) -> JobPlan:
        plan_root = root or self.release_repo
        sha = base_sha or self.git(["rev-parse", "HEAD"], cwd=plan_root).stdout.strip()
        return build_plan(plan_root, version, stream, base_sha=sha)

    def initialize_worktree(
        self, state_path: Path, state: dict[str, Any]
    ) -> tuple[Path, dict[str, Any]]:
        branch = str(state["branch"])
        worktree = Path(state["worktree"])
        if worktree.exists():
            raise SafetyError(
                f"dedicated worktree path already exists; refusing destructive cleanup: {worktree}"
            )
        worktree.parent.mkdir(parents=True, exist_ok=True)
        self.git(
            [
                "worktree",
                "add",
                "-b",
                branch,
                str(worktree),
                str(state["base_sha"]),
            ]
        )
        return worktree, self.save(
            state_path, state, phase="preparing", pending_action="prepare"
        )

    def prepare(
        self,
        state_path: Path,
        state: dict[str, Any],
        plan: JobPlan,
    ) -> tuple[Path, dict[str, Any], JobPlan]:
        worktree, state = self.initialize_worktree(state_path, state)
        worktree_plan = self.plan(
            plan.version, plan.stream, root=worktree, base_sha=plan.base_sha
        )
        if worktree_plan.digest != plan.digest:
            raise SafetyError("worktree plan differs from the approved plan")
        plan = worktree_plan
        before = plan.config_path.read_text()
        if not plan.generated_path.is_file():
            raise SafetyError("generated periodics baseline is missing")
        generated_before = plan.generated_path.read_text()
        plan.config_path.write_text(apply_config_renames(before, plan))
        self.command(
            ["make", "update", f"CONTAINER_ENGINE={self.container_engine}"],
            cwd=worktree,
        )
        validate_config_renames(before, plan.config_path.read_text(), plan)
        validate_generated_diff(generated_before, plan.generated_path.read_text(), plan)
        expected_paths = {
            str(plan.config_path.relative_to(worktree)),
            str(plan.generated_path.relative_to(worktree)),
        }
        status_text = self.git(
            ["status", "--porcelain=v2", "--untracked-files=all"], cwd=worktree
        ).stdout
        changed_paths, has_untracked = changed_paths_from_porcelain(status_text)
        if changed_paths != expected_paths or has_untracked:
            raise SafetyError(
                f"generator changed an unexpected path set: {sorted(changed_paths)}; "
                f"expected {sorted(expected_paths)}"
            )
        self.git(["diff", "--check"], cwd=worktree)
        self.git(["add", *sorted(expected_paths)], cwd=worktree)
        self.validate_pr_identity_state(state)
        title = str(state["title"])
        self.git(["commit", "-m", title], cwd=worktree)
        head_sha = self.git(["rev-parse", "HEAD"], cwd=worktree).stdout.strip()
        state = self.save(
            state_path,
            state,
            phase="prepared",
            pending_action="push",
            local_head_sha=head_sha,
        )
        return state_path, state, plan

    def push(self, state_path: Path, state: dict[str, Any]) -> dict[str, Any]:
        worktree = Path(state["worktree"])
        branch = str(state["branch"])
        expected_head = str(state["local_head_sha"])
        actual_head = self.git(["rev-parse", "HEAD"], cwd=worktree).stdout.strip()
        if actual_head != expected_head:
            raise SafetyError("worktree head changed after preparation")
        state = self.save(state_path, state, phase="prepared", pending_action="push")
        remote_before = self.git(
            ["ls-remote", FORK_REMOTE, f"refs/heads/{branch}"], check=False
        )
        if remote_before.returncode == 0 and remote_before.stdout:
            remote_sha = remote_before.stdout.split()[0]
            if remote_sha == expected_head:
                return self.save(state_path, state, phase="pushed", pending_action=None)
            raise SafetyError("remote lifecycle branch points to an unexpected commit")
        if remote_before.returncode not in {0, 2}:
            raise CommandError(
                ["git", "ls-remote", FORK_REMOTE, f"refs/heads/{branch}"],
                remote_before.returncode,
                remote_before.stderr,
            )
        try:
            self.git(["push", "-u", FORK_REMOTE, branch], cwd=worktree)
        except SafetyError:
            remote = self.git(
                ["ls-remote", FORK_REMOTE, f"refs/heads/{branch}"], check=False
            )
            remote_sha = (
                remote.stdout.split()[0]
                if remote.returncode == 0 and remote.stdout
                else ""
            )
            if remote_sha == expected_head:
                return self.save(state_path, state, phase="pushed", pending_action=None)
            self.save(
                state_path,
                state,
                phase="uncertain",
                pending_action="push",
                blocker="push result could not be reconciled",
            )
            raise SafetyError(
                "push outcome is uncertain; state retained for inspection"
            )
        return self.save(state_path, state, phase="pushed", pending_action=None)

    def find_prs(self, branch: str) -> list[dict[str, Any]]:
        return json_array(
            self.gh(
                [
                    "pr",
                    "list",
                    "--repo",
                    UPSTREAM_REPOSITORY,
                    "--state",
                    "all",
                    "--head",
                    branch,
                    "--json",
                    (
                        "number,url,state,isDraft,baseRefName,headRefName,headRefOid,mergedAt,"
                        "title,headRepository,headRepositoryOwner"
                    ),
                ]
            ).stdout,
            "PR lookup",
        )

    def validate_pr_snapshot(
        self,
        snapshot: dict[str, Any],
        state: dict[str, Any],
        *,
        require_open: bool,
    ) -> None:
        expected_base, expected_title = self.validate_pr_identity_state(state)
        if snapshot.get("title") != expected_title:
            raise SafetyError("temporary PR title changed unexpectedly")
        if snapshot.get("baseRefName") != expected_base:
            raise SafetyError("temporary rehearsal PR base changed unexpectedly")
        if snapshot.get("headRefName") != state["branch"]:
            raise SafetyError("temporary PR branch changed unexpectedly")
        if snapshot.get("headRefOid") != state["local_head_sha"]:
            raise SafetyError(
                "temporary PR head changed; stale evidence cannot be used"
            )
        if snapshot.get("isDraft") is not False:
            raise SafetyError("temporary rehearsal PR must be non-draft")
        if snapshot.get("mergedAt") is not None:
            raise SafetyError("temporary rehearsal PR was merged unexpectedly")
        if snapshot_fork_repository(snapshot) != state.get("fork_repository"):
            raise SafetyError("temporary PR belongs to an unexpected fork")
        if require_open and str(snapshot.get("state", "")).upper() != "OPEN":
            raise SafetyError("temporary rehearsal PR is not open")

    def validate_pr_identity_state(self, state: dict[str, Any]) -> tuple[str, str]:
        expected_base = state.get("base_ref")
        if expected_base != PR_BASE_REF:
            raise SafetyError("stored temporary PR base must be exactly main")
        version = state.get("version")
        stream = state.get("stream")
        if not isinstance(version, str) or not isinstance(stream, str):
            raise SafetyError("stored temporary PR version or stream is missing")
        exact_title = rehearsal_pr_title(version, stream)
        if state.get("title") != exact_title:
            raise SafetyError(
                "stored temporary PR title is not the exact generated title"
            )
        return expected_base, exact_title

    def create_pr(self, state_path: Path, state: dict[str, Any]) -> dict[str, Any]:
        expected_base, expected_title = self.validate_pr_identity_state(state)
        branch = str(state["branch"])
        existing = [
            pr
            for pr in self.find_prs(branch)
            if snapshot_fork_repository(pr) == state["fork_repository"]
        ]
        if existing:
            raise SafetyError("a PR already exists for the lifecycle branch")
        owner = str(state["fork_repository"]).split("/", 1)[0]
        body = (
            "Temporary WMCO QE rehearsal configuration for OCP "
            f"{state['version']} {state['stream']}-stream.\n\n"
            "DO NOT MERGE. Close without merging only after every expected rehearsal "
            "job succeeds for the current PR head and request."
        )
        state = self.save(state_path, state, phase="pushed", pending_action="create_pr")
        create = self.gh(
            [
                "pr",
                "create",
                "--repo",
                UPSTREAM_REPOSITORY,
                "--base",
                expected_base,
                "--head",
                f"{owner}:{branch}",
                "--title",
                expected_title,
                "--body",
                body,
            ],
            check=False,
        )
        matches = [
            pr
            for pr in self.find_prs(branch)
            if snapshot_fork_repository(pr) == state["fork_repository"]
        ]
        if len(matches) != 1:
            self.save(
                state_path,
                state,
                phase="uncertain",
                pending_action="create_pr",
                blocker="PR create result could not be reconciled",
            )
            detail = create.stderr.strip() or "no exact PR found after create attempt"
            raise SafetyError(f"PR creation outcome is uncertain: {detail}")
        snapshot = matches[0]
        self.validate_pr_snapshot(snapshot, state, require_open=True)
        return self.save(
            state_path,
            state,
            phase="pr_created",
            pending_action=None,
            pr_number=int(snapshot["number"]),
            pr_url=str(snapshot["url"]),
            pr_head_sha=str(snapshot["headRefOid"]),
        )

    def current_pr(self, state: dict[str, Any]) -> dict[str, Any]:
        self.validate_pr_identity_state(state)
        snapshot = self.pr_snapshot(state)
        self.validate_pr_snapshot(snapshot, state, require_open=True)
        return snapshot

    def pr_snapshot(self, state: dict[str, Any]) -> dict[str, Any]:
        self.validate_pr_identity_state(state)
        return json_object(
            self.gh(
                [
                    "pr",
                    "view",
                    str(state["pr_number"]),
                    "--repo",
                    UPSTREAM_REPOSITORY,
                    "--json",
                    (
                        "number,url,state,isDraft,baseRefName,headRefName,headRefOid,mergedAt,"
                        "title,headRepository,headRepositoryOwner"
                    ),
                ]
            ).stdout,
            "PR snapshot",
        )

    def adopt_pr(self, state_path: Path, state: dict[str, Any]) -> dict[str, Any]:
        self.validate_pr_identity_state(state)
        matches = [
            pr
            for pr in self.find_prs(str(state["branch"]))
            if snapshot_fork_repository(pr) == state["fork_repository"]
        ]
        if len(matches) != 1:
            return self.save(
                state_path,
                state,
                phase="uncertain",
                blocker="pending PR creation has no unique exact PR to adopt",
            )
        snapshot = matches[0]
        self.validate_pr_snapshot(snapshot, state, require_open=True)
        return self.save(
            state_path,
            state,
            phase="pr_created",
            pending_action=None,
            pr_number=int(snapshot["number"]),
            pr_url=str(snapshot["url"]),
            pr_head_sha=str(snapshot["headRefOid"]),
        )

    def comments(self, state: dict[str, Any]) -> list[dict[str, Any]]:
        return json_paginated_array(
            self.gh(
                [
                    "api",
                    f"repos/{UPSTREAM_REPOSITORY}/issues/{state['pr_number']}/comments",
                    "--paginate",
                    "--slurp",
                ]
            ).stdout,
            "PR comments",
        )

    def wait_for_notifier(
        self, state_path: Path, state: dict[str, Any]
    ) -> dict[str, Any]:
        deadline = time.monotonic() + self.notifier_timeout
        expected = tuple(state["expected_prow_jobs"])
        while time.monotonic() <= deadline:
            self.current_pr(state)
            authoritative = authoritative_notifier_comments(self.comments(state))
            if len(authoritative) > 1:
                raise SafetyError("multiple REHEARSALNOTIFIER comments are ambiguous")
            if len(authoritative) == 1:
                comment = authoritative[0]
                validate_notifier_coverage(str(comment.get("body") or ""), expected)
                login, app_slug = notifier_identity(comment)
                return self.save(
                    state_path,
                    state,
                    phase="notifier_verified",
                    notifier_comment_id=comment.get("id"),
                    notifier_url=comment.get("html_url"),
                    notifier_author=login,
                    notifier_app_slug=app_slug,
                )
            time.sleep(self.poll_seconds)
        return self.save(
            state_path,
            state,
            phase="blocked",
            blocker="expected REHEARSALNOTIFIER coverage did not arrive before timeout",
        )

    def existing_rehearsal_requests(
        self, state: dict[str, Any]
    ) -> list[dict[str, Any]]:
        return [
            comment
            for comment in self.comments(state)
            if trigger_rehearsal_commands(str(comment.get("body") or ""))
        ]

    def validate_posted_request(
        self, comment: dict[str, Any], state: dict[str, Any], command: str
    ) -> tuple[str, str]:
        if str(comment.get("body") or "").strip() != command:
            raise SafetyError("rehearsal POST response body changed")
        if type(comment.get("id")) is not int or comment["id"] <= 0:
            raise SafetyError("rehearsal POST response has no numeric comment ID")
        comment_id = comment["id"]
        created_at = str(comment.get("created_at") or "")
        if parse_timestamp(created_at) is None:
            raise SafetyError("rehearsal POST response has no usable timestamp")
        user = comment.get("user")
        if not isinstance(user, dict) or user.get("login") != state.get(
            "authenticated_login"
        ):
            raise SafetyError("rehearsal POST response has a foreign author")
        url = str(comment.get("html_url") or "")
        issue_url = str(comment.get("issue_url") or "")
        expected_pr = state.get("pr_number")
        if type(expected_pr) is not int or expected_pr <= 0:
            raise SafetyError("lifecycle state has no numeric PR number")
        expected_urls = {
            (
                f"https://github.com/openshift/release/pull/{expected_pr}"
                f"#issuecomment-{comment_id}"
            ),
            (
                f"https://github.com/openshift/release/issues/{expected_pr}"
                f"#issuecomment-{comment_id}"
            ),
        }
        expected_issue_url = (
            f"https://api.github.com/repos/{UPSTREAM_REPOSITORY}/issues/{expected_pr}"
        )
        if url not in expected_urls or issue_url != expected_issue_url:
            raise SafetyError(
                "rehearsal POST response is not pinned to the expected PR"
            )
        return created_at, url

    def request_rehearsals(
        self, state_path: Path, state: dict[str, Any]
    ) -> dict[str, Any]:
        self.current_pr(state)
        validate_recorded_notifier(self.comments(state), state)
        command = rehearsal_command(len(state["expected_prow_jobs"]))
        existing = self.existing_rehearsal_requests(state)
        if existing:
            raise SafetyError(
                "a rehearsal request already exists; refusing a duplicate trigger"
            )
        state = self.save(
            state_path,
            state,
            phase="notifier_verified",
            pending_action="request_rehearsals",
            rehearsal_command=command,
        )
        response = self.gh(
            [
                "api",
                "--method",
                "POST",
                f"repos/{UPSTREAM_REPOSITORY}/issues/{state['pr_number']}/comments",
                "-f",
                f"body={command}",
            ],
            check=False,
        )
        if response.returncode != 0:
            self.save(
                state_path,
                state,
                phase="uncertain",
                pending_action="request_rehearsals",
                blocker="rehearsal comment result could not be reconciled",
            )
            detail = response.stderr.strip() or "rehearsal request POST failed"
            raise SafetyError(f"rehearsal request outcome is uncertain: {detail}")
        try:
            comment = json_object(response.stdout, "rehearsal request POST")
            created_at, url = self.validate_posted_request(comment, state, command)
            readback = [
                candidate
                for candidate in self.comments(state)
                if candidate.get("id") == comment.get("id")
            ]
            if len(readback) != 1:
                raise SafetyError("posted rehearsal request is missing on readback")
            self.validate_posted_request(readback[0], state, command)
            if len(self.existing_rehearsal_requests(state)) != 1:
                raise SafetyError("rehearsal request is not unique after POST")
        except SafetyError as exc:
            self.save(
                state_path,
                state,
                phase="uncertain",
                pending_action="request_rehearsals",
                blocker="successful POST response could not be pinned and reconciled",
            )
            raise SafetyError(
                "rehearsal request outcome is uncertain; no foreign comment was adopted"
            ) from exc
        return self.save(
            state_path,
            state,
            phase="rehearsal_requested",
            pending_action=None,
            request_comment_id=comment.get("id"),
            request_created_at=created_at,
            request_url=url,
            request_author=state["authenticated_login"],
            requested_head_sha=state["pr_head_sha"],
        )

    def adopt_rehearsal_request(
        self, state_path: Path, state: dict[str, Any]
    ) -> dict[str, Any]:
        return self.save(
            state_path,
            state,
            phase="uncertain",
            blocker="interrupted rehearsal POST cannot safely adopt an unpinned comment",
        )

    def check_runs(self, state: dict[str, Any]) -> list[dict[str, Any]]:
        response = self.gh(
            [
                "api",
                f"repos/{UPSTREAM_REPOSITORY}/commits/{state['requested_head_sha']}/check-runs",
                "-H",
                "Accept: application/vnd.github+json",
                "--paginate",
                "--slurp",
            ]
        ).stdout
        try:
            raw_pages = json.loads(response)
        except json.JSONDecodeError as exc:
            raise SafetyError("check-runs response is invalid JSON") from exc
        if isinstance(raw_pages, dict):
            raw_pages = [raw_pages]
        if not isinstance(raw_pages, list) or not all(
            isinstance(page, dict) for page in raw_pages
        ):
            raise SafetyError("check-runs response is incomplete")
        runs: list[dict[str, Any]] = []
        for page in raw_pages:
            page_runs = page.get("check_runs")
            if not isinstance(page_runs, list) or not all(
                isinstance(item, dict) for item in page_runs
            ):
                raise SafetyError("check-runs page is incomplete")
            runs.extend(page_runs)
        return runs

    def commit_statuses(self, state: dict[str, Any]) -> list[dict[str, Any]]:
        response = self.gh(
            [
                "api",
                f"repos/{UPSTREAM_REPOSITORY}/commits/{state['requested_head_sha']}/statuses",
                "--paginate",
                "--slurp",
            ]
        ).stdout
        try:
            raw_pages = json.loads(response)
        except json.JSONDecodeError as exc:
            raise SafetyError("commit-status response is invalid JSON") from exc
        if isinstance(raw_pages, list) and all(
            isinstance(item, dict) for item in raw_pages
        ):
            statuses = raw_pages
        elif isinstance(raw_pages, list) and all(
            isinstance(page, list) for page in raw_pages
        ):
            statuses = [item for page in raw_pages for item in page]
        else:
            raise SafetyError("commit-status response is incomplete")
        for status in statuses:
            if not isinstance(status, dict):
                raise SafetyError("commit-status entry is malformed")
        return normalize_commit_statuses(statuses, str(state["requested_head_sha"]))

    def rehearsal_evidence(self, state: dict[str, Any]) -> list[dict[str, Any]]:
        return self.check_runs(state) + self.commit_statuses(state)

    def print_evidence(
        self, state: dict[str, Any], decision: MonitoringDecision
    ) -> None:
        print(f"PR: {state['pr_url']}")
        for item in decision.evidence:
            link = item.url or "no current job link"
            print(f"  {item.job}: {item.state} - {link} ({item.detail})")

    def validate_request_context(self, state: dict[str, Any]) -> None:
        if state.get("requested_head_sha") != state.get("pr_head_sha"):
            raise SafetyError("rehearsal request is not pinned to the recorded PR head")
        validate_request_comments(self.comments(state), state)

    def monitor(
        self, state_path: Path, state: dict[str, Any]
    ) -> tuple[dict[str, Any], MonitoringDecision]:
        self.validate_pr_identity_state(state)
        self.validate_request_context(state)
        deadline = time.monotonic() + self.monitor_timeout
        expected = tuple(state["expected_prow_jobs"])
        state = self.save(state_path, state, phase="monitoring")
        latest: MonitoringDecision | None = None
        while time.monotonic() <= deadline:
            self.current_pr(state)
            self.validate_request_context(state)
            latest = evaluate_check_runs(
                self.rehearsal_evidence(state),
                expected,
                head_sha=str(state["requested_head_sha"]),
                request_created_at=str(state["request_created_at"]),
                pr_number=int(state["pr_number"]),
            )
            self.print_evidence(state, latest)
            if latest.outcome == "success":
                return state, latest
            if latest.outcome == "blocked":
                state = self.save(
                    state_path,
                    state,
                    phase="blocked",
                    blocker="one or more expected rehearsal jobs did not succeed",
                )
                return state, latest
            time.sleep(self.poll_seconds)
        if latest is None:
            latest = MonitoringDecision("waiting", ())
        state = self.save(
            state_path,
            state,
            phase="blocked",
            blocker="expected rehearsal jobs did not all succeed before timeout",
        )
        return state, latest

    def close_pr(
        self,
        state_path: Path,
        state: dict[str, Any],
        decision: MonitoringDecision,
    ) -> dict[str, Any]:
        self.validate_pr_identity_state(state)
        if not decision.may_close:
            raise SafetyError(
                "close is forbidden without complete expected-job success"
            )
        self.current_pr(state)
        self.validate_request_context(state)
        refreshed = evaluate_check_runs(
            self.rehearsal_evidence(state),
            tuple(state["expected_prow_jobs"]),
            head_sha=str(state["requested_head_sha"]),
            request_created_at=str(state["request_created_at"]),
            pr_number=int(state["pr_number"]),
        )
        if not refreshed.may_close:
            self.print_evidence(state, refreshed)
            raise SafetyError(
                "fresh pre-close evidence is not complete, unique, current success"
            )
        self.current_pr(state)
        self.validate_request_context(state)
        state = self.save(
            state_path, state, phase="monitoring", pending_action="close_without_merge"
        )
        response = self.gh(
            [
                "api",
                "--method",
                "PATCH",
                f"repos/{UPSTREAM_REPOSITORY}/pulls/{state['pr_number']}",
                "-f",
                "state=closed",
            ],
            check=False,
        )
        snapshot_text = self.gh(
            [
                "pr",
                "view",
                str(state["pr_number"]),
                "--repo",
                UPSTREAM_REPOSITORY,
                "--json",
                (
                    "number,url,state,isDraft,baseRefName,headRefName,headRefOid,mergedAt,"
                    "title,headRepository,headRepositoryOwner"
                ),
            ],
            check=False,
        )
        if snapshot_text.returncode != 0:
            self.save(
                state_path,
                state,
                phase="uncertain",
                blocker="close result could not be read back",
            )
            raise SafetyError(
                "close outcome is uncertain; PR must be inspected manually"
            )
        snapshot = json_object(snapshot_text.stdout, "closed PR snapshot")
        self.validate_pr_snapshot(snapshot, state, require_open=False)
        if str(snapshot.get("state", "")).upper() != "CLOSED":
            self.save(
                state_path,
                state,
                phase="uncertain",
                blocker="PR remains open after close attempt",
            )
            detail = response.stderr.strip() or "PR remains open"
            raise SafetyError(f"close outcome is uncertain: {detail}")
        return self.save(
            state_path,
            state,
            phase="closed",
            pending_action=None,
            closed_at=dt.datetime.now(dt.timezone.utc).isoformat(),
        )

    def reconcile_close(
        self, state_path: Path, state: dict[str, Any]
    ) -> dict[str, Any]:
        self.validate_pr_identity_state(state)
        snapshot = self.pr_snapshot(state)
        try:
            self.validate_pr_snapshot(snapshot, state, require_open=False)
        except SafetyError as exc:
            return self.save(
                state_path,
                state,
                phase="uncertain",
                blocker=f"pending close snapshot is unsafe: {exc}",
            )
        pr_state = str(snapshot.get("state") or "").upper()
        if pr_state == "CLOSED":
            return self.save(
                state_path,
                state,
                phase="closed",
                pending_action=None,
                closed_at=dt.datetime.now(dt.timezone.utc).isoformat(),
            )
        return self.save(
            state_path,
            state,
            phase="uncertain",
            blocker="pending close did not produce a closed, unmerged PR",
        )

    def continue_from_pr(
        self, state_path: Path, state: dict[str, Any]
    ) -> dict[str, Any]:
        phase = str(state.get("phase"))
        if phase == "pr_created":
            state = self.wait_for_notifier(state_path, state)
            if state["phase"] == "blocked":
                return state
            phase = str(state["phase"])
        if phase == "notifier_verified":
            state = self.request_rehearsals(state_path, state)
            phase = str(state["phase"])
        if phase in {"rehearsal_requested", "monitoring"}:
            state, decision = self.monitor(state_path, state)
            if decision.may_close:
                return self.close_pr(state_path, state, decision)
            return state
        return state

    def start(
        self,
        version: str,
        stream: str,
        *,
        approved_plan_digest: str,
        run_id: str | None = None,
    ) -> tuple[Path, dict[str, Any]]:
        plan = self.plan(version, stream)
        if not secrets.compare_digest(plan.digest, approved_plan_digest):
            raise SafetyError(
                "current plan does not match --approved-plan-digest; review plan again"
            )
        checkout_status = self.git(
            ["status", "--porcelain=v1", "--untracked-files=all"]
        ).stdout
        if checkout_status.strip():
            raise SafetyError("release checkout must be clean before start")
        fork_repository, actor = self.preflight_repository(prepare_runtime=False)
        self.git(["fetch", UPSTREAM_REMOTE, "main"])
        upstream_sha = self.git(["rev-parse", f"{UPSTREAM_REMOTE}/main"]).stdout.strip()
        if upstream_sha != plan.base_sha:
            raise SafetyError(
                "upstream/main changed after the reviewed plan; re-run and approve plan"
            )
        self.preflight_runtime()
        run_id = validate_run_id(run_id) if run_id else new_run_id()
        branch = self.branch_name(version, stream, run_id)
        worktree = self.worktree_path(version, stream, run_id)
        state_path = self.state_path(version, stream, run_id)
        self.assert_no_duplicate(branch, state_path, fork_repository)
        state: dict[str, Any] = {
            "schema": STATE_SCHEMA,
            "phase": "initializing",
            "pending_action": "create_worktree",
            "run_id": run_id,
            "version": plan.version,
            "stream": plan.stream,
            "branch": branch,
            "worktree": str(worktree),
            "base_sha": plan.base_sha,
            "base_ref": PR_BASE_REF,
            "title": rehearsal_pr_title(plan.version, plan.stream),
            "approved_plan_digest": plan.digest,
            "selected_jobs": list(plan.selected),
            "renamed_jobs": plan.renamed,
            "expected_prow_jobs": list(plan.expected_prow_jobs),
            "fork_repository": fork_repository,
            "authenticated_login": actor,
            "container_engine": self.container_engine,
        }
        state = self.save(state_path, state)
        print(f"Run ID: {run_id}")
        print(f"State: {state_path}", flush=True)
        state_path, state, _ = self.prepare(state_path, state, plan)
        state = self.push(state_path, state)
        state = self.create_pr(state_path, state)
        return state_path, self.continue_from_pr(state_path, state)

    def resume(
        self, version: str, stream: str, *, run_id: str
    ) -> tuple[Path, dict[str, Any]]:
        fork_repository, actor = self.preflight_repository(prepare_runtime=False)
        state_path = self.state_path(version, stream, validate_run_id(run_id))
        state = load_state(state_path)
        if state.get("version") != version or state.get("stream") != stream:
            raise SafetyError("resume arguments do not match lifecycle state")
        if state.get("run_id") != run_id:
            raise SafetyError("resume run ID does not match lifecycle state")
        if state.get("fork_repository") != fork_repository:
            raise SafetyError("origin fork changed since lifecycle initialization")
        if state.get("authenticated_login") != actor:
            raise SafetyError("authenticated GitHub user changed since initialization")
        phase = str(state.get("phase"))
        if phase in {"closed", "blocked", "uncertain"}:
            return state_path, state
        if phase in {"initializing", "preparing"}:
            state = self.save(
                state_path,
                state,
                phase="blocked",
                blocker="local preparation was interrupted; inspect the preserved worktree",
            )
            return state_path, state
        if phase == "prepared" and state.get("pending_action") == "push":
            state = self.push(state_path, state)
            phase = str(state["phase"])
        if phase == "pushed" and state.get("pending_action") == "create_pr":
            state = self.adopt_pr(state_path, state)
            phase = str(state["phase"])
            if phase == "uncertain":
                return state_path, state
        if phase == "pushed":
            state = self.create_pr(state_path, state)
            phase = str(state["phase"])
        if (
            phase == "notifier_verified"
            and state.get("pending_action") == "request_rehearsals"
        ):
            state = self.adopt_rehearsal_request(state_path, state)
            phase = str(state["phase"])
            if phase == "uncertain":
                return state_path, state
        if (
            phase == "monitoring"
            and state.get("pending_action") == "close_without_merge"
        ):
            state = self.reconcile_close(state_path, state)
            return state_path, state
        return state_path, self.continue_from_pr(state_path, state)


def print_plan(plan: JobPlan) -> None:
    print(f"OCP minor release: {plan.version}")
    print(f"Stream: {plan.stream}-stream")
    print(f"Reviewed base SHA: {plan.base_sha}")
    print(f"Payload definition: {plan.payload_definition}")
    print("Configured latest WMCO catalog paths: verified")
    for workflow, path in sorted(plan.catalog_paths.items()):
        print(f"  {workflow}: {path}")
        manifest = plan.closure_manifests[workflow]
        print(
            f"    closure: {manifest['identity']} schema={manifest['schema']} "
            f"topology=sha256:{manifest['topology_digest']} "
            f"commands=sha256:{manifest['command_inventory_digest']}"
        )
        for template in plan.catalog_templates[workflow]:
            print(
                f"    {template['role']}: {template['identity']} "
                f"sha256:{template['digest']}"
            )
    print("Selected jobs:")
    for job in plan.selected:
        print(f"  {job} -> {plan.renamed[job]}")
    print("Expected rehearsal checks:")
    for job in plan.expected_prow_jobs:
        print(f"  ci/rehearse/{job}")
    print(f"Authoritative command: {rehearsal_command(len(plan.selected))}")
    print(f"Plan digest: {plan.digest}")


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Safely plan or run temporary WMCO QE Prow rehearsals."
    )
    subparsers = parser.add_subparsers(dest="action", required=True)
    for action in ("plan", "start", "resume"):
        command = subparsers.add_parser(action)
        command.add_argument(
            "ocp_version", help="OCP minor release, for example 4.21 or 5.0"
        )
        command.add_argument("stream", help="z-stream or y-stream")
        command.add_argument(
            "--release-repo",
            required=True,
            type=Path,
            help="existing full clone of an openshift/release fork",
        )
        if action != "plan":
            command.add_argument(
                "--confirm-live-writes",
                metavar=LIVE_CONFIRMATION,
                help="required literal confirmation for live GitHub and Prow writes",
            )
            command.add_argument("--poll-seconds", type=int, default=30)
            command.add_argument("--notifier-timeout", type=int, default=600)
            command.add_argument("--monitor-timeout", type=int, default=21600)
            command.add_argument(
                "--container-engine",
                choices=("podman", "docker"),
                default=os.environ.get("CONTAINER_ENGINE", "podman"),
            )
            command.add_argument(
                "--run-id",
                required=action == "resume",
                help="collision-resistant run ID printed by start (required for resume)",
            )
        if action == "start":
            command.add_argument(
                "--approved-plan-digest",
                required=True,
                help="exact digest from the reviewed plan output",
            )
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    version = validate_ocp_version(args.ocp_version)
    stream = validate_stream(args.stream)
    lifecycle = Lifecycle(
        args.release_repo,
        Runner(),
        poll_seconds=getattr(args, "poll_seconds", 30),
        notifier_timeout=getattr(args, "notifier_timeout", 600),
        monitor_timeout=getattr(args, "monitor_timeout", 21600),
        container_engine=getattr(args, "container_engine", "podman"),
    )
    try:
        if args.action == "plan":
            print_plan(lifecycle.plan(version, stream))
            return 0
        if args.confirm_live_writes != LIVE_CONFIRMATION:
            raise SafetyError(
                "live mode requires --confirm-live-writes " + LIVE_CONFIRMATION
            )
        if args.action == "start":
            state_path, state = lifecycle.start(
                version,
                stream,
                approved_plan_digest=args.approved_plan_digest,
                run_id=args.run_id,
            )
        else:
            state_path, state = lifecycle.resume(version, stream, run_id=args.run_id)
        print(f"State: {state_path}")
        print(f"Phase: {state['phase']}")
        if state.get("pr_url"):
            print(f"PR: {state['pr_url']}")
        if state.get("blocker"):
            print(f"Blocked: {state['blocker']}")
        return 0 if state["phase"] == "closed" else 2
    except SafetyError as exc:
        print(f"Safety stop: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
