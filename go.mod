module github.com/charliek/shed

go 1.27.0

require (
	cel.dev/cel-go v0.33.2
	github.com/anmitsu/go-shlex v0.0.0-20200514113438-38f4b401e2be
	github.com/charliek/shed/sdk v0.0.0
	github.com/creack/pty v1.1.24
	github.com/distribution/reference v0.6.0
	github.com/firecracker-microvm/firecracker-go-sdk v1.0.1-0.20251224190957-6fb280e993d4
	github.com/gliderlabs/ssh v0.3.8
	github.com/go-chi/chi/v5 v5.5.0
	github.com/google/go-containerregistry v0.22.1
	github.com/hugelgupf/p9 v0.3.0
	github.com/mdlayher/vsock v1.4.0
	github.com/sirupsen/logrus v1.10.2
	github.com/spf13/cobra v1.10.2
	github.com/vishvananda/netlink v1.3.1
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.62.0
	golang.org/x/sync v0.25.0
	golang.org/x/sys v0.50.0
	golang.org/x/term v0.48.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	cel.dev/expr v0.25.1 // indirect
	github.com/antlr4-go/antlr/v4 v4.13.1 // indirect
	github.com/asaskevich/govalidator v0.0.0-20230301143203-a9d515a09cc2 // indirect
	github.com/containerd/fifo v1.1.0 // indirect
	github.com/containernetworking/cni v1.3.0 // indirect
	github.com/containernetworking/plugins v1.9.0 // indirect
	github.com/docker/cli v29.7.2+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.9.3 // indirect
	github.com/go-openapi/analysis v0.23.0 // indirect
	github.com/go-openapi/errors v0.22.1 // indirect
	github.com/go-openapi/jsonpointer v0.21.0 // indirect
	github.com/go-openapi/jsonreference v0.21.0 // indirect
	github.com/go-openapi/loads v0.22.0 // indirect
	github.com/go-openapi/runtime v0.24.0 // indirect
	github.com/go-openapi/spec v0.21.0 // indirect
	github.com/go-openapi/strfmt v0.23.0 // indirect
	github.com/go-openapi/swag v0.23.0 // indirect
	github.com/go-openapi/validate v0.24.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/mdlayher/socket v0.8.0 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	github.com/oklog/ulid v1.3.1 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/opentracing/opentracing-go v1.2.0 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/u-root/uio v0.0.0-20240224005618-d2acac8f3701 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	go.mongodb.org/mongo-driver v1.14.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/exp v0.0.0-20240823005443-9b4947da3948 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20251202230838-ff82c1b0f217 // indirect
	google.golang.org/protobuf v1.36.10 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gotest.tools/v3 v3.5.2 // indirect
)

replace github.com/charliek/shed/sdk => ./sdk
