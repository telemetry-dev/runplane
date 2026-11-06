package sysbox

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"

	"github.com/runplane/runplane/internal/sandbox"
	"github.com/runplane/runplane/internal/sandbox/guest"
)

// labelProbe marks the short containers of Probe.
const labelProbe = "dev.runplane.probe"

type ProbeOptions struct {
	Docker  *client.Client
	Profile Profile
	// DataDir is the data folder of the driver. The probe container carries
	// it, so that the recovery of another runplane leaves it alone.
	DataDir string
	DNS     []netip.Addr // empty: the Docker default
}

// Probe starts a short container on the runplane network, with the sandbox
// runtime and the firewall rules, and fetches https://api.github.com. The
// doctor uses it for the sandbox internet check. It runs curl in the newest
// runplane image on this server, and returns sandbox.ErrNoProbe while the
// server has none.
func Probe(ctx context.Context, o ProbeOptions) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	img, err := probeImage(ctx, o.Docker)
	if err != nil {
		return err
	}
	pids := int64(64)
	res, err := o.Docker.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      img,
			Entrypoint: []string{"curl", "-fsS", "-o", "/dev/null", "--max-time", "20", "https://api.github.com"},
			Labels:     map[string]string{labelProbe: "true", guest.LabelManaged: "true", labelData: o.DataDir},
			Tty:        true, // plain logs, without the stream headers
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(networkName),
			Runtime:     o.Profile.runtime,
			DNS:         o.DNS,
			Resources: container.Resources{
				NanoCPUs:     1e9,
				Memory:       256 << 20,
				PidsLimit:    &pids,
				CgroupParent: o.Profile.cgroupParent,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create probe container: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		o.Docker.ContainerRemove(ctx, res.ID, client.ContainerRemoveOptions{Force: true})
	}()
	wait := o.Docker.ContainerWait(ctx, res.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := o.Docker.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start probe container: %w", err)
	}
	select {
	case r := <-wait.Result:
		if r.StatusCode == 0 {
			return nil
		}
		return fmt.Errorf("curl https://api.github.com exited with code %d: %s", r.StatusCode, probeOutput(ctx, o.Docker, res.ID))
	case err := <-wait.Error:
		return fmt.Errorf("wait for probe container: %w", err)
	}
}

// probeImage is the newest image with the runplane label. The base images
// have it, and the images FROM them inherit it.
func probeImage(ctx context.Context, docker *client.Client) (string, error) {
	res, err := docker.ImageList(ctx, client.ImageListOptions{
		Filters: client.Filters{}.Add("label", guest.LabelManaged+"=true"),
	})
	if err != nil {
		return "", fmt.Errorf("list images: %w", err)
	}
	if len(res.Items) == 0 {
		return "", sandbox.ErrNoProbe
	}
	newest := slices.MaxFunc(res.Items, func(a, b image.Summary) int { return cmp.Compare(a.Created, b.Created) })
	return newest.ID, nil
}

func probeOutput(ctx context.Context, docker *client.Client, id string) string {
	logs, err := docker.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return err.Error()
	}
	defer logs.Close()
	out, _ := io.ReadAll(io.LimitReader(logs, 4096))
	return strings.TrimSpace(string(out))
}
