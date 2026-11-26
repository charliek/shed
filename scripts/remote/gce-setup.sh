#!/usr/bin/env bash
# gce-setup.sh --print: the one-time Google Cloud setup that baking a shed
# machine image needs (`make gce-image`, scripts/build-gce-image.sh).
#
# It PRINTS the gcloud commands and runs none of them. Read each block, then
# run it yourself. Everything project-specific comes from flags, so this file
# holds no project, address or account.
#
# This is the image bake's setup only. A shed-server holds no cloud
# credential and makes no cloud call: its machines come from the account
# service, which holds the credential, the network and the firewall rules
# for the machines it makes (see docs/reference/remote-backend.md). The
# commands here are run by the person who bakes the image, with their own
# gcloud sign-in; they create no service account, role or key.
#
# The bake boots a builder VM on a network of its own, reaches it over SSH
# from the machine that runs `make gce-image`, saves its disk as an image and
# deletes the builder. That needs a network and subnet, and one firewall rule
# that admits tcp:22 from the baking machine to the builder's network tag.
#
# Usage:
#   gce-setup.sh --print --project PROJECT --region REGION \
#                --source-range CIDR [--name NAME] [--subnet-range CIDR]
#
#   --project        Google Cloud project ID.
#   --region         region of the zone the builder runs in (e.g. us-central1).
#   --source-range   the baking machine's public address as a CIDR
#                    (x.x.x.x/32): the only source allowed to reach port 22
#                    of the builder.
#   --name           name of the network, subnet, firewall rule and network
#                    tag (default shed-bake): 1-40 of a-z, 0-9 and '-',
#                    starting with a letter, not ending in '-'.
#   --subnet-range   the subnet's range (default 10.10.0.0/24).
set -euo pipefail

usage() {
	sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
	exit 2
}

fail() { echo "gce-setup.sh: $*" >&2; exit 2; }

# value FLAG [VALUE]: the flag's value, or exit 2 when the flag came last.
value() {
	[[ $# -ge 2 ]] || fail "$1 needs a value"
	printf '%s' "$2"
}

print=0
project="" region="" source_range=""
name="shed-bake" subnet_range="10.10.0.0/24"
while [[ $# -gt 0 ]]; do
	case "$1" in
	--print) print=1 ;;
	--project) project=$(value "$@") || exit 2; shift ;;
	--region) region=$(value "$@") || exit 2; shift ;;
	--source-range) source_range=$(value "$@") || exit 2; shift ;;
	--name) name=$(value "$@") || exit 2; shift ;;
	--subnet-range) subnet_range=$(value "$@") || exit 2; shift ;;
	-h | --help) usage ;;
	*) echo "gce-setup.sh: unknown argument: $1" >&2; usage ;;
	esac
	shift
done

if [[ $print -ne 1 ]]; then
	echo "usage: $0 --print --project P --region R --source-range CIDR   (prints the commands; runs nothing)" >&2
	exit 2
fi

# valid_cidr CIDR: a dotted IPv4 address with octets of 0-255 and a prefix
# length of 0-32, with no leading zeros (so there is one spelling of each).
valid_cidr() {
	local octet='(0|[1-9][0-9]{0,2})' o
	[[ $1 =~ ^$octet\.$octet\.$octet\.$octet/(0|[1-9][0-9]?)$ ]] || return 1
	for o in "${BASH_REMATCH[@]:1:4}"; do
		((o <= 255)) || return 1
	done
	((BASH_REMATCH[5] <= 32))
}

[[ $project =~ ^[a-z][-a-z0-9]{4,28}[a-z0-9]$ ]] || fail "--project: not a project ID: '$project'"
[[ $region =~ ^[a-z]+-[a-z]+[0-9]+$ ]] || fail "--region: not a region (e.g. us-central1): '$region'"
[[ $name =~ ^[a-z]([-a-z0-9]{0,38}[a-z0-9])?$ ]] || fail "--name: 1-40 of a-z, 0-9, '-', starting with a letter: '$name'"
valid_cidr "$source_range" || fail "--source-range: not an IPv4 CIDR: '$source_range'"
[[ ${source_range##*/} != 0 ]] || fail "--source-range: a /0 range opens the builder to the internet; name the baking machine's address"
valid_cidr "$subnet_range" || fail "--subnet-range: not an IPv4 CIDR: '$subnet_range'"

rule="$name-ssh"

cat <<EOF
# shed machine image bake: one-time setup in project '$project'.
# Nothing below has been run. Read each block, then run it yourself, signed
# in to gcloud as a person who may create networks and firewall rules here.

PROJECT=$project
REGION=$region

########################################################################
# 1. The Compute Engine API.
########################################################################
gcloud services enable compute.googleapis.com --project="\$PROJECT"

########################################################################
# 2. A custom-mode network and one subnet for the builder, IPv4 only. Never
#    use the project's default network: it allows internal traffic and SSH
#    from anywhere.
########################################################################
gcloud compute networks create $name --project="\$PROJECT" \\
  --subnet-mode=custom --bgp-routing-mode=regional
gcloud compute networks subnets create $name --project="\$PROJECT" \\
  --network=$name --region="\$REGION" --range=$subnet_range --stack-type=IPV4_ONLY

########################################################################
# 3. The one ingress rule: tcp:22 from the baking machine only, to
#    instances tagged $name. Keep the source range current when the baking
#    machine's address changes:
#      gcloud compute firewall-rules update $rule --project="\$PROJECT" --source-ranges=<address>/32
########################################################################
gcloud compute firewall-rules create $rule --project="\$PROJECT" \\
  --network=$name --direction=INGRESS --priority=1000 --action=ALLOW \\
  --rules=tcp:22 --source-ranges=$source_range --target-tags=$name

########################################################################
# 4. Optional, recommended for a project that only runs shed machines: stop
#    new VMs from getting the default compute service account by mistake.
########################################################################
# gcloud iam service-accounts disable \\
#   "\$(gcloud projects describe "\$PROJECT" --format='value(projectNumber)')-compute@developer.gserviceaccount.com" \\
#   --project="\$PROJECT"

# What \`make gce-image\` reads (process environment, or --env-file):
#   SHED_GCE_PROJECT=$project
#   SHED_GCE_ZONE=<a zone in $region, e.g. $region-a>
#   SHED_GCE_NETWORK=$name
#   SHED_GCE_SUBNET=$name
#   SHED_GCE_BAKE_TAG=$name
#
# The image lands in the family shed-full-amd64 (or shed-base-amd64) of this
# project. Letting the account service's cloud adapter use it is that
# service's own setup, not this script's.
EOF
