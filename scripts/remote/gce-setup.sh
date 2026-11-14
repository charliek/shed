#!/usr/bin/env bash
# gce-setup.sh --print: the one-time Google Cloud setup one shed-server
# environment needs for `default_backend: remote` with `provider: gce`.
#
# It PRINTS the gcloud commands and runs none of them. Read each block, then
# run it yourself. Everything project-specific comes from flags, so this file
# holds no project, address or account.
#
# One environment is one server's slice of a project: its own network and
# subnet, firewall rule, service account and key. The custom role is shared
# by every environment in the project.
#
# Usage:
#   gce-setup.sh --print --project PROJECT --env ENV --region REGION \
#                --source-range CIDR [--subnet-range CIDR] [--role-id ID] \
#                [--key-out PATH] [--firewall-auto]
#
#   --project        Google Cloud project ID.
#   --env            environment name (shed `remote.environment`): 1-20 of
#                    a-z, 0-9 and '-', starting with a letter, not ending in '-'.
#   --region         region of the zone the server uses (e.g. us-central1).
#   --source-range   the shed-server's public address as a CIDR (x.x.x.x/32);
#                    the only source allowed to reach port 22 of the sheds.
#   --subnet-range   the environment subnet's range (default 10.10.0.0/24).
#   --role-id        custom role ID (default shedServer).
#   --key-out        where the base64 key is written (default
#                    ./shed-<env>-key.b64, mode 0600).
#   --firewall-auto  also print the two extra roles `remote.firewall.mode:
#                    auto` needs, so the server can point the firewall rule at
#                    its own changing address. Leave it out for mode static:
#                    the server then holds no permission to change any rule.
#
# The role's permission list is internal/remote/provider/gce.Permissions, and
# the --firewall-auto roles' are gce.FirewallAutoRulePermissions and
# gce.FirewallAutoNetworkPermissions; a Go test keeps them equal.
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

print=0 firewall_auto=0
project="" env="" region="" source_range=""
subnet_range="10.10.0.0/24" role_id="shedServer" key_out=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	--print) print=1 ;;
	--project) project=$(value "$@") || exit 2; shift ;;
	--env) env=$(value "$@") || exit 2; shift ;;
	--region) region=$(value "$@") || exit 2; shift ;;
	--source-range) source_range=$(value "$@") || exit 2; shift ;;
	--subnet-range) subnet_range=$(value "$@") || exit 2; shift ;;
	--role-id) role_id=$(value "$@") || exit 2; shift ;;
	--key-out) key_out=$(value "$@") || exit 2; shift ;;
	--firewall-auto) firewall_auto=1 ;;
	-h | --help) usage ;;
	*) echo "gce-setup.sh: unknown argument: $1" >&2; usage ;;
	esac
	shift
done

if [[ $print -ne 1 ]]; then
	echo "usage: $0 --print --project P --env E --region R --source-range CIDR   (prints the commands; runs nothing)" >&2
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
[[ $env =~ ^[a-z]([-a-z0-9]{0,18}[a-z0-9])?$ ]] || fail "--env: 1-20 of a-z, 0-9, '-', starting with a letter: '$env'"
[[ $region =~ ^[a-z]+-[a-z]+[0-9]+$ ]] || fail "--region: not a region (e.g. us-central1): '$region'"
valid_cidr "$source_range" || fail "--source-range: not an IPv4 CIDR: '$source_range'"
[[ ${source_range##*/} != 0 ]] || fail "--source-range: a /0 range opens every shed to the internet; name the server's address"
valid_cidr "$subnet_range" || fail "--subnet-range: not an IPv4 CIDR: '$subnet_range'"
[[ $role_id =~ ^[a-zA-Z0-9_.]{3,64}$ ]] || fail "--role-id: 3-64 of letters, digits, '_' and '.': '$role_id'"
[[ $firewall_auto -ne 1 || ${#role_id} -le 52 ]] || fail "--role-id: at most 52 characters with --firewall-auto (two role IDs are derived from it)"
key_out=${key_out:-./shed-$env-key.b64}
[[ $key_out =~ ^[-A-Za-z0-9_./~]+$ ]] || fail "--key-out: use a plain path: '$key_out'"

name="shed-$env"     # network, subnet, network tag and service account
rule="shed-$env-ssh" # provider.FirewallRuleName
sa="$name@$project.iam.gserviceaccount.com"

# The exact permission list (internal/remote/provider/gce.Permissions).
permissions=(
	compute.disks.create
	compute.disks.delete
	compute.disks.get
	compute.disks.list
	compute.disks.setLabels
	compute.firewalls.list
	compute.images.get
	compute.images.getFromFamily
	compute.images.useReadOnly
	compute.instances.create
	compute.instances.delete
	compute.instances.get
	compute.instances.getGuestAttributes
	compute.instances.list
	compute.instances.setLabels
	compute.instances.setMetadata
	compute.instances.setScheduling
	compute.instances.setTags
	compute.instances.start
	compute.instances.stop
	compute.networks.get
	compute.subnetworks.get
	compute.subnetworks.use
	compute.subnetworks.useExternalIp
	compute.zoneOperations.get
)
perm_csv=$(IFS=,; echo "${permissions[*]}")

# --firewall-auto only. firewalls.patch needs permissions on two resources:
# the rule itself and the network it is on.
# (internal/remote/provider/gce.FirewallAutoRulePermissions)
auto_rule_permissions=(
	compute.firewalls.get
	compute.firewalls.update
)
# (internal/remote/provider/gce.FirewallAutoNetworkPermissions)
auto_network_permissions=(
	compute.globalOperations.get
	compute.networks.updatePolicy
)
auto_rule_csv=$(IFS=,; echo "${auto_rule_permissions[*]}")
auto_network_csv=$(IFS=,; echo "${auto_network_permissions[*]}")
rule_role_id="${role_id}FirewallRule"
network_role_id="${role_id}FirewallNet"

cat <<EOF
# shed remote backend: one-time setup for environment '$env' in project '$project'.
# Nothing below has been run. Read each block, then run it yourself.

PROJECT=$project
REGION=$region

########################################################################
# 1. The Compute Engine API.
########################################################################
gcloud services enable compute.googleapis.com --project="\$PROJECT"

########################################################################
# 2. A custom-mode network and one subnet for this environment, IPv4 only.
#    No rule allows VM-to-VM traffic, so sheds cannot reach each other
#    (the 'isolated' network profile holds by construction). Never use the
#    project's default network: it allows internal traffic and SSH from
#    anywhere.
########################################################################
gcloud compute networks create $name --project="\$PROJECT" \\
  --subnet-mode=custom --bgp-routing-mode=regional
gcloud compute networks subnets create $name --project="\$PROJECT" \\
  --network=$name --region="\$REGION" --range=$subnet_range --stack-type=IPV4_ONLY

########################################################################
# 3. The one ingress rule: tcp:22 from the shed-server only, to instances
#    tagged $name. The server never creates or deletes rules. It reads this
#    one, and the network's other ingress rules, and refuses to create or
#    start a shed while they are not as made here (503
#    FIREWALL_CHECK_FAILED). With remote.firewall.mode: static (the default)
#    its role can only list rules, so keep the source range current here
#    and in the server's SHED_REMOTE_SOURCE_RANGES; see --firewall-auto for
#    a server whose address changes.
########################################################################
gcloud compute firewall-rules create $rule --project="\$PROJECT" \\
  --network=$name --direction=INGRESS --priority=1000 --action=ALLOW \\
  --rules=tcp:22 --source-ranges=$source_range --target-tags=$name

########################################################################
# 4. The custom role, shared by every environment in the project: exactly
#    what the shed-server's Compute client calls (create once per project;
#    later environments skip this block, or use 'update' after a shed
#    upgrade changes the list).
########################################################################
gcloud iam roles create $role_id --project="\$PROJECT" \\
  --title="shed-server (remote backend)" \\
  --description="What shed-server needs to manage one VM per shed." \\
  --stage=GA \\
  --permissions=$perm_csv

########################################################################
# 5. This environment's service account, holding only that role. The VMs
#    themselves get no service account at all.
########################################################################
gcloud iam service-accounts create $name --project="\$PROJECT" \\
  --display-name="shed-server, environment $env"
gcloud projects add-iam-policy-binding "\$PROJECT" \\
  --member="serviceAccount:$sa" \\
  --role="projects/\$PROJECT/roles/$role_id" --condition=None

EOF

if [[ $firewall_auto -eq 1 ]]; then
	cat <<EOF
########################################################################
# 5b. Only for remote.firewall.mode: auto (you passed --firewall-auto).
#     Two more custom roles, so the server can patch the source ranges of
#     its own rule when its public address changes.
#
#     What a holder of these roles can do: change ANY field of the rule
#     $rule (its sources, ports and targets), not only its source ranges.
#     Google has no permission narrower than compute.firewalls.update. A
#     stolen key could therefore open port 22 of this environment's sheds,
#     or other ports, to any address. It cannot create or delete rules, and
#     the binding below is limited to the one rule by an IAM condition.
#     shed-server itself only ever sends the rule's sourceRanges, one /32,
#     and refuses new sheds whenever the rule is not exactly as block 3
#     made it. If you do not need address following, do not run this block.
#
#     The condition: Google's IAM documentation ("Resource attributes for
#     IAM Conditions") lists compute.googleapis.com/Firewall among the
#     resource types whose resource.name a condition may test, with the
#     form projects/PROJECT_ID/global/firewalls/RULE. It does NOT list
#     networks or operations, so compute.networks.updatePolicy and
#     compute.globalOperations.get go in a second role bound without a
#     condition. Those two alone change no rule: every firewall change
#     also needs a compute.firewalls.* permission on the rule. This split
#     was taken from the documentation and has not been exercised against
#     a live project by the shed test suite. After running it, confirm that
#     the server's log shows "patched rule $rule" when its address
#     changes, and that updating any OTHER firewall rule with the server's
#     key is refused.
########################################################################
gcloud iam roles create $rule_role_id --project="\$PROJECT" \\
  --title="shed-server firewall rule (firewall.mode auto)" \\
  --description="Patch the source ranges of one shed firewall rule." \\
  --stage=GA \\
  --permissions=$auto_rule_csv
gcloud iam roles create $network_role_id --project="\$PROJECT" \\
  --title="shed-server firewall network (firewall.mode auto)" \\
  --description="The network and operation half of a firewall rule patch." \\
  --stage=GA \\
  --permissions=$auto_network_csv
gcloud projects add-iam-policy-binding "\$PROJECT" \\
  --member="serviceAccount:$sa" \\
  --role="projects/\$PROJECT/roles/$rule_role_id" \\
  --condition='expression=resource.name == "projects/$project/global/firewalls/$rule",title=$rule only,description=shed environment $env may update its own firewall rule and no other'
gcloud projects add-iam-policy-binding "\$PROJECT" \\
  --member="serviceAccount:$sa" \\
  --role="projects/\$PROJECT/roles/$network_role_id" --condition=None

EOF
fi

cat <<EOF
########################################################################
# 6. Its key, written base64-encoded to a file only you can read and never
#    shown. Give it to the server as GOOGLE_CREDENTIALS_BASE64 (the file's
#    content) or SHED_GCE_CREDENTIALS_FILE (the path), then delete any
#    other copy.
#
#    The file must not exist yet: an existing file would keep its own
#    mode, whatever the umask. With noclobber the first redirect creates
#    it (mode 0600) or stops the block before any key is made; '>|' then
#    writes the file this block created.
########################################################################
( umask 077 && set -o noclobber && : > $key_out && \\
  gcloud iam service-accounts keys create /dev/stdout \\
    --iam-account="$sa" --project="\$PROJECT" | base64 | tr -d '\\n' >| $key_out )

########################################################################
# 7. Optional, recommended for a project that only runs sheds: stop new VMs
#    from getting the default compute service account by mistake.
########################################################################
# gcloud iam service-accounts disable \\
#   "\$(gcloud projects describe "\$PROJECT" --format='value(projectNumber)')-compute@developer.gserviceaccount.com" \\
#   --project="\$PROJECT"

########################################################################
# 8. Spending guards, by hand in the console: a budget alert on the project
#    (Billing > Budgets & alerts), and a lowered regional CPUS quota
#    (IAM & Admin > Quotas) so a runaway server cannot outrun the budget.
########################################################################

# Server config for this environment (values go in the process environment,
# never the config file):
#   SHED_REMOTE_ENV=$env
#   SHED_GCE_PROJECT=$project
#   SHED_GCE_ZONE=<a zone in $region, e.g. $region-a>
#   SHED_GCE_NETWORK=$name
#   SHED_GCE_SUBNET=$name
#   SHED_GCE_CREDENTIALS_FILE=$key_out
#   SHED_REMOTE_SOURCE_RANGES=$source_range
# (the last is remote.firewall.source_ranges_env, for firewall.mode: static;
# it must equal the rule's source ranges, comma-separated)
EOF
