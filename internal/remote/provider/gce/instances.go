package gce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charliek/shed/internal/remote/guest"
	"github.com/charliek/shed/internal/remote/provider"
)

// Metadata switches every instance gets besides the shed keys.
const (
	MetadataEnableGuestAttributes = "enable-guest-attributes"
	MetadataBlockProjectSSHKeys   = "block-project-ssh-keys"
	// MetadataEnableOSLogin is set to FALSE on every instance. An instance
	// with no value inherits the project's, and with OS Login on the
	// guest agent rewrites the guest's sshd, NSS and PAM set-up for
	// cloud-managed logins. A shed has one login, the link user's key,
	// and its sshd is the image's. The image builder sets the same value.
	MetadataEnableOSLogin = "enable-oslogin"
)

// reservedMetadata are the switches a caller may not set.
var reservedMetadata = map[string]bool{
	MetadataEnableGuestAttributes: true,
	MetadataBlockProjectSSHKeys:   true,
	MetadataEnableOSLogin:         true,
}

// fingerprintRetries is how often setLabels and setMetadata read again and
// retry after another writer changed the instance in between.
const fingerprintRetries = 3

// Ref format: "<zone>/<instance name>".
var refRegex = regexp.MustCompile(`^([a-z][a-z0-9-]*[a-z0-9])/([a-z]([-a-z0-9]{0,61}[a-z0-9])?)$`)

func makeRef(zone, name string) provider.Ref { return provider.Ref(zone + "/" + name) }

func parseRef(op string, ref provider.Ref) (zone, name string, err error) {
	m := refRegex.FindStringSubmatch(string(ref))
	if m == nil {
		return "", "", provider.Errorf(provider.ErrInvalid, op, "ref %q is not a GCE instance ref", ref)
	}
	return m[1], m[2], nil
}

func (c *Client) instancePath(zone, name string) string {
	return c.zonePath(zone) + "instances/" + name
}

// int64String reads an int64 the API sends as a JSON string (or a number).
type int64String int64

func (n *int64String) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*n = int64String(v)
	return nil
}

func (n int64String) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatInt(int64(n), 10) + `"`), nil
}

type duration struct {
	Seconds int64String `json:"seconds"`
	Nanos   int         `json:"nanos,omitempty"`
}

type metadataItem struct {
	Key   string  `json:"key"`
	Value *string `json:"value,omitempty"`
}

type metadata struct {
	Kind        string         `json:"kind,omitempty"`
	Fingerprint string         `json:"fingerprint,omitempty"`
	Items       []metadataItem `json:"items"`
}

// gceInstance is the subset of the instance resource the client reads.
type gceInstance struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	Status             string            `json:"status"`
	Zone               string            `json:"zone"`
	MachineType        string            `json:"machineType"`
	CreationTimestamp  string            `json:"creationTimestamp"`
	LastStartTimestamp string            `json:"lastStartTimestamp"`
	LastStopTimestamp  string            `json:"lastStopTimestamp"`
	Labels             map[string]string `json:"labels"`
	LabelFingerprint   string            `json:"labelFingerprint"`
	Metadata           metadata          `json:"metadata"`
	NetworkInterfaces  []struct {
		NetworkIP     string `json:"networkIP"`
		AccessConfigs []struct {
			NatIP string `json:"natIP"`
		} `json:"accessConfigs"`
	} `json:"networkInterfaces"`
	Disks []struct {
		Boot         bool        `json:"boot"`
		DiskSizeGb   int64String `json:"diskSizeGb"`
		Architecture string      `json:"architecture"`
		Source       string      `json:"source"`
	} `json:"disks"`
	Scheduling     json.RawMessage `json:"scheduling"`
	ResourceStatus struct {
		Scheduling struct {
			TerminationTimestamp string `json:"terminationTimestamp"`
		} `json:"scheduling"`
	} `json:"resourceStatus"`
}

// mapState turns a GCE status into a provider.State.
func mapState(status string) provider.State {
	switch status {
	case "PENDING", "PROVISIONING", "STAGING":
		return provider.StateStarting
	case "RUNNING":
		return provider.StateRunning
	case "PENDING_STOP", "STOPPING", "SUSPENDING", "DEPROVISIONING":
		// PENDING_STOP is a graceful shutdown under way; DEPROVISIONING is
		// the tear-down after the VM has halted.
		return provider.StateStopping
	case "TERMINATED", "STOPPED":
		return provider.StateStopped
	}
	// SUSPENDED, REPAIRING and anything new.
	return provider.StateUnknown
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// toInstance converts the API's instance to the provider's view.
func (c *Client) toInstance(g *gceInstance) provider.Instance {
	zone := lastSegment(g.Zone)
	if zone == "" {
		zone = c.cfg.Zone
	}
	inst := provider.Instance{
		Ref:         makeRef(zone, g.Name),
		Name:        g.Name,
		State:       mapState(g.Status),
		Labels:      maps.Clone(g.Labels),
		Metadata:    map[string]string{},
		Zone:        zone,
		MachineType: lastSegment(g.MachineType),
		CreatedAt:   parseTime(g.CreationTimestamp),
		StartedAt:   parseTime(g.LastStartTimestamp),
		StoppedAt:   parseTime(g.LastStopTimestamp),
	}
	if inst.Labels == nil {
		inst.Labels = map[string]string{}
	}
	for _, it := range g.Metadata.Items {
		if strings.HasPrefix(it.Key, "shed-") && it.Value != nil {
			inst.Metadata[it.Key] = *it.Value
		}
	}
	if len(g.NetworkInterfaces) > 0 {
		ni := g.NetworkInterfaces[0]
		inst.InternalAddress = ni.NetworkIP
		if len(ni.AccessConfigs) > 0 {
			inst.Address = ni.AccessConfigs[0].NatIP
		}
	}
	if inst.State != provider.StateRunning && inst.State != provider.StateStarting {
		// A stopped VM has released its ephemeral address; never hand out
		// one the API still echoes.
		inst.Address = ""
	}
	for _, d := range g.Disks {
		if d.Boot {
			inst.DiskGB = int(d.DiskSizeGb)
			inst.Arch = archFromDisk(d.Architecture)
			break
		}
	}
	for _, s := range c.cfg.Sizes {
		if s.MachineType == inst.MachineType {
			inst.CPUs, inst.MemoryMB = s.CPUs, s.MemoryMB
			if s.Arch != "" {
				inst.Arch = s.Arch
			}
			break
		}
	}
	if len(g.Scheduling) > 0 {
		var sched struct {
			MaxRunDuration *duration `json:"maxRunDuration"`
		}
		if json.Unmarshal(g.Scheduling, &sched) == nil && sched.MaxRunDuration != nil {
			inst.MaxRun = time.Duration(sched.MaxRunDuration.Seconds)*time.Second + time.Duration(sched.MaxRunDuration.Nanos)
		}
	}
	if inst.State == provider.StateRunning {
		inst.PlannedStopAt = parseTime(g.ResourceStatus.Scheduling.TerminationTimestamp)
	}
	if spec, err := inst.ShedSpec(); err == nil {
		inst.Image = spec.Image
	}
	return inst
}

func archFromDisk(a string) string {
	switch a {
	case "X86_64":
		return provider.ArchAMD64
	case "ARM64":
		return provider.ArchARM64
	}
	return ""
}

// getRaw reads one instance.
func (c *Client) getRaw(ctx context.Context, op string, zone, name string) (*gceInstance, error) {
	var g gceInstance
	err := c.call(ctx, request{op: op, method: http.MethodGet, path: c.instancePath(zone, name)}, &g)
	if err != nil {
		return nil, asProviderError(op, err)
	}
	return &g, nil
}

// mine reads an instance for a mutating call and refuses one that belongs to
// another environment.
func (c *Client) mine(ctx context.Context, op string, ref provider.Ref) (zone string, g *gceInstance, err error) {
	zone, name, err := parseRef(op, ref)
	if err != nil {
		return "", nil, err
	}
	g, err = c.getRaw(ctx, op, zone, name)
	if err != nil {
		return "", nil, err
	}
	if env := g.Labels[provider.LabelEnv]; env != c.cfg.Env {
		return "", nil, provider.Errorf(provider.ErrForeign, op, "%s has %s=%q, this server is %q", ref, provider.LabelEnv, env, c.cfg.Env)
	}
	return zone, g, nil
}

// Get implements provider.Provider.
func (c *Client) Get(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
	zone, name, err := parseRef("get", ref)
	if err != nil {
		return provider.Instance{}, err
	}
	g, err := c.getRaw(ctx, "get", zone, name)
	if err != nil {
		return provider.Instance{}, err
	}
	return c.toInstance(g), nil
}

// List implements provider.Provider. It reads every page of the zone's
// instances, filtered on the shed_env label when sel.Env is set.
func (c *Client) List(ctx context.Context, sel provider.Selector) ([]provider.Instance, error) {
	q := url.Values{}
	if sel.Env != "" {
		if !provider.ValidEnv(sel.Env) {
			return nil, provider.Errorf(provider.ErrInvalid, "list", "environment %q is not a valid name", sel.Env)
		}
		q.Set("filter", fmt.Sprintf(`labels.%s = "%s"`, provider.LabelEnv, sel.Env))
	}
	var out []provider.Instance
	err := c.listPages(ctx, "list", c.zonePath(c.cfg.Zone)+"instances", q, func(raw json.RawMessage) error {
		var items []gceInstance
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		for i := range items {
			inst := c.toInstance(&items[i])
			// The filter is the API's; check it again so a filter the API
			// misread can never hand back another environment's VM.
			if sel.Env != "" && inst.Labels[provider.LabelEnv] != sel.Env {
				continue
			}
			out = append(out, inst)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// listPages walks a paged list call. each receives every page's "items".
func (c *Client) listPages(ctx context.Context, op, path string, q url.Values, each func(json.RawMessage) error) error {
	seen := map[string]bool{}
	token := ""
	for {
		pq := url.Values{}
		for k, v := range q {
			pq[k] = v
		}
		pq.Set("maxResults", strconv.Itoa(listPageSize))
		if token != "" {
			pq.Set("pageToken", token)
		}
		var page struct {
			Items         json.RawMessage `json:"items"`
			NextPageToken string          `json:"nextPageToken"`
		}
		if err := c.call(ctx, request{op: op, method: http.MethodGet, path: path, query: pq}, &page); err != nil {
			return asProviderError(op, err)
		}
		if len(page.Items) > 0 {
			if err := each(page.Items); err != nil {
				return &provider.Error{Op: op, Detail: "decode list page", Err: err}
			}
		}
		if page.NextPageToken == "" {
			return nil
		}
		if seen[page.NextPageToken] {
			return provider.Errorf(nil, op, "the API repeated page token %q", page.NextPageToken)
		}
		seen[page.NextPageToken] = true
		token = page.NextPageToken
	}
}

// insertBody is the instances.insert request.
type insertBody struct {
	Name              string             `json:"name"`
	MachineType       string             `json:"machineType"`
	Labels            map[string]string  `json:"labels"`
	Tags              tags               `json:"tags"`
	Metadata          metadata           `json:"metadata"`
	Disks             []attachedDisk     `json:"disks"`
	NetworkInterfaces []networkInterface `json:"networkInterfaces"`
	Scheduling        scheduling         `json:"scheduling"`
	ServiceAccounts   []any              `json:"serviceAccounts"`
}

type tags struct {
	Items []string `json:"items"`
}

type attachedDisk struct {
	Type             string         `json:"type"`
	Boot             bool           `json:"boot"`
	AutoDelete       bool           `json:"autoDelete"`
	InitializeParams initializeDisk `json:"initializeParams"`
}

type initializeDisk struct {
	SourceImage string            `json:"sourceImage"`
	DiskSizeGb  int64String       `json:"diskSizeGb"`
	DiskType    string            `json:"diskType"`
	Labels      map[string]string `json:"labels"`
}

type networkInterface struct {
	Network       string         `json:"network"`
	Subnetwork    string         `json:"subnetwork,omitempty"`
	StackType     string         `json:"stackType"`
	AccessConfigs []accessConfig `json:"accessConfigs"`
}

type accessConfig struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	NetworkTier string `json:"networkTier"`
}

type scheduling struct {
	ProvisioningModel         string    `json:"provisioningModel"`
	OnHostMaintenance         string    `json:"onHostMaintenance"`
	AutomaticRestart          bool      `json:"automaticRestart"`
	InstanceTerminationAction string    `json:"instanceTerminationAction,omitempty"`
	MaxRunDuration            *duration `json:"maxRunDuration,omitempty"`
}

// sourceImage turns Spec.Image into the insert's sourceImage: a bare name is
// an image in the client's project; a "projects/..." path is kept.
func (c *Client) sourceImage(image string) string {
	if strings.HasPrefix(image, "projects/") {
		return image
	}
	return c.projectPath() + "global/images/" + image
}

// buildInsert returns the instances.insert body for a validated spec.
func (c *Client) buildInsert(spec provider.Spec) insertBody {
	diskLabels := map[string]string{}
	for _, k := range provider.DiskLabelKeys {
		if v, ok := spec.Labels[k]; ok {
			diskLabels[k] = v
		}
	}
	md := map[string]string{
		MetadataEnableGuestAttributes: "TRUE",
		MetadataBlockProjectSSHKeys:   "TRUE",
		MetadataEnableOSLogin:         "FALSE",
	}
	maps.Copy(md, spec.Metadata)
	body := insertBody{
		Name:        spec.Name,
		MachineType: "zones/" + c.cfg.Zone + "/machineTypes/" + spec.Size.MachineType,
		Labels:      maps.Clone(spec.Labels),
		Tags:        tags{Items: []string{provider.NetworkTag(c.cfg.Env)}},
		Metadata:    metadata{Items: metadataItems(md)},
		Disks: []attachedDisk{{
			Type:       "PERSISTENT",
			Boot:       true,
			AutoDelete: true,
			InitializeParams: initializeDisk{
				SourceImage: c.sourceImage(spec.Image),
				DiskSizeGb:  int64String(spec.DiskGB),
				DiskType:    "zones/" + c.cfg.Zone + "/diskTypes/" + c.cfg.DiskType,
				Labels:      diskLabels,
			},
		}},
		NetworkInterfaces: []networkInterface{{
			Network:    c.networkURL,
			Subnetwork: c.subnetURL,
			StackType:  "IPV4_ONLY",
			AccessConfigs: []accessConfig{{
				Type:        "ONE_TO_ONE_NAT",
				Name:        "External NAT",
				NetworkTier: c.cfg.NetworkTier,
			}},
		}},
		Scheduling: scheduling{
			ProvisioningModel: "STANDARD",
			OnHostMaintenance: "MIGRATE",
			AutomaticRestart:  true,
		},
		ServiceAccounts: []any{},
	}
	if spec.MaxRun > 0 {
		body.Scheduling.InstanceTerminationAction = "STOP"
		body.Scheduling.MaxRunDuration = durationOf(spec.MaxRun)
	}
	return body
}

// durationOf rounds a run limit up to whole seconds; GCE takes at least 30.
func durationOf(d time.Duration) *duration {
	secs := int64((d + time.Second - 1) / time.Second)
	if secs < 30 {
		secs = 30
	}
	return &duration{Seconds: int64String(secs)}
}

func metadataItems(md map[string]string) []metadataItem {
	keys := make([]string, 0, len(md))
	for k := range md {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	items := make([]metadataItem, 0, len(keys))
	for _, k := range keys {
		v := md[k]
		items = append(items, metadataItem{Key: k, Value: &v})
	}
	return items
}

// Create implements provider.Provider.
func (c *Client) Create(ctx context.Context, spec provider.Spec) (provider.Pending, error) {
	if err := spec.Validate(); err != nil {
		return provider.Pending{}, err
	}
	if env := spec.Labels[provider.LabelEnv]; env != c.cfg.Env {
		return provider.Pending{}, provider.Errorf(provider.ErrInvalid, "create", "spec is for environment %q, this provider serves %q", env, c.cfg.Env)
	}
	for k := range spec.Metadata {
		if reservedMetadata[k] {
			return provider.Pending{}, provider.Errorf(provider.ErrInvalid, "create", "metadata key %q is reserved", k)
		}
	}
	// Metadata is readable by every process in the guest and by anyone who
	// may read the instance. provider.NewSpec strips repo credentials; a
	// Spec built another way must not get them past this point.
	if shed, err := provider.DecodeShedSpec(spec.Metadata[provider.MetadataShedSpec]); err == nil && provider.StripRepoUserinfo(shed.Repo) != shed.Repo {
		return provider.Pending{}, provider.Errorf(provider.ErrInvalid, "create", "metadata %s holds a repo URL with credentials", provider.MetadataShedSpec)
	}
	ref := makeRef(c.cfg.Zone, spec.Name)
	o, err := c.mutate(ctx, "create", http.MethodPost, c.zonePath(c.cfg.Zone)+"instances", nil, c.buildInsert(spec))
	if errors.Is(err, provider.ErrExists) {
		g, gerr := c.getRaw(ctx, "create", c.cfg.Zone, spec.Name)
		if errors.Is(gerr, provider.ErrNotFound) {
			// Deleted in between. The name is not taken, so this is not
			// ErrExists: the outcome is unknown and the caller retries.
			return provider.Pending{}, provider.Errorf(nil, "create", "%s existed when the insert was refused and is gone now; try again", spec.Name)
		}
		if gerr != nil {
			return provider.Pending{}, gerr
		}
		if g.Labels[provider.LabelID] != spec.Labels[provider.LabelID] || g.Labels[provider.LabelEnv] != c.cfg.Env {
			return provider.Pending{}, provider.Errorf(provider.ErrExists, "create", "%s is taken by %s=%q", spec.Name, provider.LabelID, g.Labels[provider.LabelID])
		}
		return provider.Pending{Ref: ref, Want: provider.StateRunning}, nil
	}
	if err != nil {
		return provider.Pending{}, err
	}
	return provider.Pending{Ref: ref, Operation: o.Name, Want: provider.StateRunning}, nil
}

// Start implements provider.Provider.
func (c *Client) Start(ctx context.Context, ref provider.Ref, opts provider.StartOptions) (provider.Pending, error) {
	zone, g, err := c.mine(ctx, "start", ref)
	if err != nil {
		return provider.Pending{}, err
	}
	switch st := mapState(g.Status); st {
	case provider.StateRunning, provider.StateStarting:
		return provider.Pending{Ref: ref, Want: provider.StateRunning}, nil
	case provider.StateStopped:
	default:
		return provider.Pending{}, provider.Errorf(provider.ErrBusy, "start", "%s is %s", ref, g.Status)
	}
	if opts.MaxRun > 0 {
		if err := c.setRunLimit(ctx, zone, g, opts.MaxRun); err != nil {
			return provider.Pending{}, err
		}
	}
	o, err := c.mutate(ctx, "start", http.MethodPost, c.instancePath(zone, g.Name)+"/start", nil, nil)
	if err != nil {
		return provider.Pending{}, err
	}
	return provider.Pending{Ref: ref, Operation: o.Name, Want: provider.StateRunning}, nil
}

// setRunLimit changes a stopped instance's maxRunDuration when it differs
// from want, keeping its other scheduling fields, and waits for the change.
func (c *Client) setRunLimit(ctx context.Context, zone string, g *gceInstance, want time.Duration) error {
	sched := map[string]any{}
	if len(g.Scheduling) > 0 {
		if err := json.Unmarshal(g.Scheduling, &sched); err != nil {
			return &provider.Error{Op: "start", Detail: "decode scheduling", Err: err}
		}
	}
	d := durationOf(want)
	if cur, ok := sched["maxRunDuration"].(map[string]any); ok {
		if fmt.Sprint(cur["seconds"]) == strconv.FormatInt(int64(d.Seconds), 10) {
			return nil
		}
	}
	sched["maxRunDuration"] = d
	sched["instanceTerminationAction"] = "STOP"
	o, err := c.mutate(ctx, "start", http.MethodPost, c.instancePath(zone, g.Name)+"/setScheduling", nil, sched)
	if err != nil {
		return err
	}
	return c.runOperation(ctx, "start", o)
}

// Stop implements provider.Provider.
func (c *Client) Stop(ctx context.Context, ref provider.Ref) (provider.Pending, error) {
	zone, g, err := c.mine(ctx, "stop", ref)
	if err != nil {
		return provider.Pending{}, err
	}
	switch mapState(g.Status) {
	case provider.StateStopped, provider.StateStopping:
		return provider.Pending{Ref: ref, Want: provider.StateStopped}, nil
	case provider.StateRunning:
	default:
		return provider.Pending{}, provider.Errorf(provider.ErrBusy, "stop", "%s is %s", ref, g.Status)
	}
	o, err := c.mutate(ctx, "stop", http.MethodPost, c.instancePath(zone, g.Name)+"/stop", nil, nil)
	if err != nil {
		return provider.Pending{}, err
	}
	return provider.Pending{Ref: ref, Operation: o.Name, Want: provider.StateStopped}, nil
}

// Destroy implements provider.Provider. The boot disk goes with the instance
// (autoDelete). A delete the API refuses because another operation is
// running is ErrBusy.
func (c *Client) Destroy(ctx context.Context, ref provider.Ref) (provider.Pending, error) {
	done := provider.Pending{Ref: ref, Want: provider.StateAbsent}
	zone, g, err := c.mine(ctx, "destroy", ref)
	if errors.Is(err, provider.ErrNotFound) {
		return done, nil
	}
	if err != nil {
		return provider.Pending{}, err
	}
	o, err := c.mutate(ctx, "destroy", http.MethodDelete, c.instancePath(zone, g.Name), nil, nil)
	if errors.Is(err, provider.ErrNotFound) {
		return done, nil
	}
	if err != nil {
		return provider.Pending{}, err
	}
	done.Operation = o.Name
	return done, nil
}

// Wait implements provider.Provider. It follows the operation with
// zoneOperations.wait, then reads the instance once a second until it is
// settled.
func (c *Client) Wait(ctx context.Context, p provider.Pending) (provider.Instance, error) {
	zone, name, err := parseRef("wait", p.Ref)
	if err != nil {
		return provider.Instance{}, err
	}
	if p.Operation != "" {
		o, err := c.waitOperation(ctx, "wait", zone, p.Operation)
		switch {
		case errors.Is(err, errOpUnknown):
			// Follow the instance alone.
		case err != nil:
			return provider.Instance{}, err
		default:
			ferr := o.failure("wait")
			if p.Want == provider.StateAbsent && errors.Is(ferr, provider.ErrNotFound) {
				// A delete that found nothing to delete (another delete got
				// there first) is the outcome that was asked for.
				ferr = nil
			}
			if ferr != nil {
				return provider.Instance{}, ferr
			}
		}
	}
	for {
		g, err := c.getRaw(ctx, "wait", zone, name)
		if errors.Is(err, provider.ErrNotFound) {
			if p.Want == provider.StateAbsent {
				return provider.Instance{}, nil
			}
			return provider.Instance{}, err
		}
		if err != nil {
			return provider.Instance{}, err
		}
		inst := c.toInstance(g)
		if inst.State.Settled() {
			if p.Want == "" || inst.State == p.Want {
				return inst, nil
			}
			return inst, provider.Errorf(provider.ErrUnexpectedState, "wait", "%s is %s, want %s", p.Ref, inst.State, p.Want)
		}
		if err := sleep(ctx, c.cfg.PollInterval); err != nil {
			return provider.Instance{}, &provider.Error{Op: "wait", Err: err}
		}
	}
}

// SetLabels implements provider.Provider.
func (c *Client) SetLabels(ctx context.Context, ref provider.Ref, labels map[string]string) error {
	for k, v := range labels {
		if k == "" || !provider.ValidLabelValue(k) || k[0] < 'a' || k[0] > 'z' {
			return provider.Errorf(provider.ErrInvalid, "set_labels", "label key %q is not valid", k)
		}
		if !provider.ValidLabelValue(v) {
			return provider.Errorf(provider.ErrInvalid, "set_labels", "label %s has an invalid value %q", k, v)
		}
	}
	for attempt := 0; ; attempt++ {
		zone, g, err := c.mine(ctx, "set_labels", ref)
		if err != nil {
			return err
		}
		merged := maps.Clone(g.Labels)
		if merged == nil {
			merged = map[string]string{}
		}
		changed := false
		for k, v := range labels {
			cur, ok := merged[k]
			switch k {
			case provider.LabelSchema, provider.LabelEnv, provider.LabelID:
				if cur != v {
					return provider.Errorf(provider.ErrInvalid, "set_labels", "label %s cannot be changed", k)
				}
			}
			if !ok || cur != v {
				merged[k] = v
				changed = true
			}
		}
		if !changed {
			return nil
		}
		if len(merged) > 64 {
			return provider.Errorf(provider.ErrInvalid, "set_labels", "an instance takes at most 64 labels")
		}
		body := map[string]any{"labels": merged, "labelFingerprint": g.LabelFingerprint}
		o, err := c.mutate(ctx, "set_labels", http.MethodPost, c.instancePath(zone, g.Name)+"/setLabels", nil, body)
		if err == nil {
			err = c.runOperation(ctx, "set_labels", o)
		}
		if errors.Is(err, errFingerprint) && attempt < fingerprintRetries {
			continue
		}
		return err
	}
}

// SetMetadata implements provider.Provider. Keys that do not begin "shed-"
// are refused, and the instance's other keys (the GCE switches) are kept.
func (c *Client) SetMetadata(ctx context.Context, ref provider.Ref, md map[string]string) error {
	for k := range md {
		if !strings.HasPrefix(k, "shed-") {
			return provider.Errorf(provider.ErrInvalid, "set_metadata", "metadata key %q does not begin \"shed-\"", k)
		}
	}
	for attempt := 0; ; attempt++ {
		zone, g, err := c.mine(ctx, "set_metadata", ref)
		if err != nil {
			return err
		}
		cur := map[string]string{}
		for _, it := range g.Metadata.Items {
			if it.Value != nil {
				cur[it.Key] = *it.Value
			} else {
				cur[it.Key] = ""
			}
		}
		next := maps.Clone(cur)
		for k, v := range md {
			if v == "" {
				delete(next, k)
			} else {
				next[k] = v
			}
		}
		if maps.Equal(cur, next) {
			return nil
		}
		body := metadata{Kind: "compute#metadata", Fingerprint: g.Metadata.Fingerprint, Items: metadataItems(next)}
		o, err := c.mutate(ctx, "set_metadata", http.MethodPost, c.instancePath(zone, g.Name)+"/setMetadata", nil, body)
		if err == nil {
			err = c.runOperation(ctx, "set_metadata", o)
		}
		if errors.Is(err, errFingerprint) && attempt < fingerprintRetries {
			continue
		}
		return err
	}
}

// GuestHostKey implements provider.HostKeyReader. It reads guest attribute
// shed/host-key and returns its first two fields, "ssh-ed25519 AAAA...".
func (c *Client) GuestHostKey(ctx context.Context, ref provider.Ref) (string, error) {
	zone, name, err := parseRef("guest_host_key", ref)
	if err != nil {
		return "", err
	}
	q := url.Values{"variableKey": {guest.HostKeyAttrNamespace + "/" + guest.HostKeyAttrKey}}
	var reply struct {
		VariableValue string `json:"variableValue"`
	}
	err = c.call(ctx, request{op: "guest_host_key", method: http.MethodGet, path: c.instancePath(zone, name) + "/getGuestAttributes", query: q}, &reply)
	if err != nil {
		return "", asProviderError("guest_host_key", err)
	}
	fields := strings.Fields(reply.VariableValue)
	if len(fields) < 2 {
		return "", provider.Errorf(provider.ErrNotFound, "guest_host_key", "%s has published no host key", ref)
	}
	return fields[0] + " " + fields[1], nil
}
