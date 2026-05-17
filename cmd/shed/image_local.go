// Local-filesystem image subcommands: history, save, load. These read
// the on-disk OCI image store directly rather than going through the
// HTTP API — useful both for `shed image save -o image.tar | ...`
// piping and for keeping the response shape small (no JSON for tar
// streams). Pattern mirrors the legacy `shed image install` flow that
// also operated against images_dir directly.

package main

import (
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/vmimage"
)

var (
	imageSaveOutput string
	imageLoadInput  string
)

var imageHistoryCmd = &cobra.Command{
	Use:   "history <tag-or-digest>",
	Short: "Show the layer history of an image",
	Long: `Print the OCI layer stack of an image: digest, size, source
variant, and any per-layer history annotations. Reads the local on-disk
OCI store at images_dir.`,
	Args: cobra.ExactArgs(1),
	RunE: runImageHistory,
}

var imageSaveCmd = &cobra.Command{
	Use:   "save <tag-or-digest>",
	Short: "Export an image as an OCI layout tar",
	Long: `Save an image (manifest + config + layers + kernel/initrd
blobs) as a tar stream. The output is a valid OCI image-layout-v1 tar
that crane, oras, and skopeo can consume.

  shed image save shed-vz-full -o image.tar
  shed image save shed-vz-full -o - | ssh other 'shed image load -i -'`,
	Args: cobra.ExactArgs(1),
	RunE: runImageSave,
}

var imageLoadCmd = &cobra.Command{
	Use:   "load",
	Short: "Import an image from an OCI layout tar",
	Long: `Load an image previously written by 'shed image save' (or any
compatible OCI image-layout-v1 tar). Blobs already present in the
store are deduplicated by digest.

  shed image load -i image.tar
  cat image.tar | shed image load -i -`,
	Args: cobra.NoArgs,
	RunE: runImageLoad,
}

func init() {
	imageSaveCmd.Flags().StringVarP(&imageSaveOutput, "output", "o", "", "Output file (- for stdout, required)")
	_ = imageSaveCmd.MarkFlagRequired("output")
	imageLoadCmd.Flags().StringVarP(&imageLoadInput, "input", "i", "", "Input file (- for stdin, required)")
	_ = imageLoadCmd.MarkFlagRequired("input")

	imageCmd.AddCommand(imageHistoryCmd)
	imageCmd.AddCommand(imageSaveCmd)
	imageCmd.AddCommand(imageLoadCmd)
}

// loadLocalManager reads the active server config and returns a
// vmimage.Manager bound to the backend's image store.
func loadLocalManager() (*vmimage.Manager, error) {
	cfg, err := config.LoadServerConfig()
	if err != nil {
		return nil, fmt.Errorf("loading server config: %w", err)
	}
	switch cfg.DefaultBackend {
	case "vz":
		if cfg.VZ == nil {
			return nil, fmt.Errorf("server config has default_backend=vz but no vz: block")
		}
		return vmimage.NewManager(cfg.VZ, nil), nil
	case "firecracker":
		if cfg.Firecracker == nil {
			return nil, fmt.Errorf("server config has default_backend=firecracker but no firecracker: block")
		}
		return vmimage.NewManager(cfg.Firecracker, nil), nil
	case "":
		return nil, fmt.Errorf("server config has no default_backend set")
	default:
		return nil, fmt.Errorf("unsupported default_backend %q", cfg.DefaultBackend)
	}
}

func runImageHistory(_ *cobra.Command, args []string) error {
	mgr, err := loadLocalManager()
	if err != nil {
		return err
	}
	layers, err := mgr.ImageHistory(args[0])
	if err != nil {
		return fmt.Errorf("history: %w", err)
	}
	if jsonFlag {
		return outputJSON(layers)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "LAYER\tDIGEST\tSIZE\tCREATED\tCREATED BY")
	for _, l := range layers {
		created := ""
		if !l.CreatedAt.IsZero() {
			created = l.CreatedAt.Format(time.RFC3339)
		}
		createdBy := l.CreatedBy
		if len(createdBy) > 60 {
			createdBy = createdBy[:57] + "..."
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", l.Index, vmimage.ShortDigest(l.Digest), formatSize(l.Size), created, createdBy)
	}
	w.Flush()
	return nil
}

func runImageSave(_ *cobra.Command, args []string) error {
	mgr, err := loadLocalManager()
	if err != nil {
		return err
	}
	var out *os.File
	if imageSaveOutput == "-" {
		out = os.Stdout
	} else {
		out, err = os.Create(imageSaveOutput)
		if err != nil {
			return fmt.Errorf("creating output: %w", err)
		}
		defer out.Close()
	}
	if err := mgr.SaveImage(args[0], out); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	if imageSaveOutput != "-" {
		printSuccess("Saved %s to %s", args[0], imageSaveOutput)
	}
	return nil
}

func runImageLoad(_ *cobra.Command, _ []string) error {
	mgr, err := loadLocalManager()
	if err != nil {
		return err
	}
	var in *os.File
	if imageLoadInput == "-" {
		in = os.Stdin
	} else {
		in, err = os.Open(imageLoadInput)
		if err != nil {
			return fmt.Errorf("opening input: %w", err)
		}
		defer in.Close()
	}
	added, err := mgr.LoadImage(in)
	if err != nil {
		return fmt.Errorf("load: %w", err)
	}
	if jsonFlag {
		return outputJSON(map[string]any{"added": added})
	}
	printSuccess("Loaded %d image(s)", len(added))
	for _, d := range added {
		fmt.Printf("  %s\n", vmimage.ShortDigest(d))
	}
	return nil
}
