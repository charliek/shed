package gce

// Permissions is every IAM permission the Client's calls need, and nothing
// else. scripts/remote/gce-setup.sh prints a custom role with exactly this
// list; a test keeps the two equal.
//
//	instances.insert            compute.instances.create, compute.disks.create,
//	                            compute.images.useReadOnly, compute.subnetworks.use,
//	                            compute.subnetworks.useExternalIp,
//	                            compute.instances.setMetadata, compute.instances.setLabels,
//	                            compute.instances.setTags, compute.disks.setLabels
//	instances.get               compute.instances.get
//	instances.list              compute.instances.list
//	instances.start             compute.instances.start
//	instances.stop              compute.instances.stop
//	instances.delete            compute.instances.delete (the boot disk goes with it)
//	instances.setLabels         compute.instances.setLabels
//	instances.setMetadata       compute.instances.setMetadata
//	instances.setScheduling     compute.instances.setScheduling
//	instances.getGuestAttributes compute.instances.getGuestAttributes
//	zoneOperations.wait         compute.zoneOperations.get
//	images.get                  compute.images.get
//	images.getFromFamily        compute.images.getFromFamily
//	disks.list, get, delete     compute.disks.list, compute.disks.get, compute.disks.delete
//	networks.get                compute.networks.get
//	subnetworks.get             compute.subnetworks.get
//	firewalls.list              compute.firewalls.list
var Permissions = []string{
	"compute.disks.create",
	"compute.disks.delete",
	"compute.disks.get",
	"compute.disks.list",
	"compute.disks.setLabels",
	"compute.firewalls.list",
	"compute.images.get",
	"compute.images.getFromFamily",
	"compute.images.useReadOnly",
	"compute.instances.create",
	"compute.instances.delete",
	"compute.instances.get",
	"compute.instances.getGuestAttributes",
	"compute.instances.list",
	"compute.instances.setLabels",
	"compute.instances.setMetadata",
	"compute.instances.setScheduling",
	"compute.instances.setTags",
	"compute.instances.start",
	"compute.instances.stop",
	"compute.networks.get",
	"compute.subnetworks.get",
	"compute.subnetworks.use",
	"compute.subnetworks.useExternalIp",
	"compute.zoneOperations.get",
}
