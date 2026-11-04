package host

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestProvider(t *testing.T) {
	tests := []struct {
		dmi, hostname string
		provider      string
		virtual       bool
	}{
		{"hetzner-cloud", "ubuntu-8gb-fsn1-1", providerHetzner, true},
		{"digitalocean", "droplet-1", providerDigitalOcean, true},
		{"vultr", "vultr-guest", providerVultr, true},
		{"linode", "localhost", providerLinode, true},
		{"aws-nitro", "ip-172-31-5-10", providerEC2, false},
		{"aws-xen", "ip-172-31-5-11", providerEC2, true},
		{"gce", "instance-1", providerGCE, false},
		{"azure", "vm1", providerAzure, true},
		// Hyper-V outside Azure has the same vendor and product.
		{"hyperv", "lab", "", true},
		{"openstack", "vps-1a2b3c4d.vps.ovh.net", providerOVH, true},
		{"openstack", "ns3012345.ip-51-68-1.eu", providerOVH, true},
		{"openstack", "web-1", "", true},
		{"qemu", "vmi1234567.contaboserver.net", providerContabo, true},
		{"qemu", "vmi1234567", providerContabo, true},
		{"qemu", "v2202301234567890123.megasrv.de", providerNetcup, true},
		{"qemu", "builder", "", true},
		{"netcup", "builder", providerNetcup, true},
		{"bare-metal", "Ubuntu-2404-noble-amd64-base", "", false},
	}
	for _, tt := range tests {
		board := readDMI(os.DirFS("testdata/dmi/" + tt.dmi))
		if got := board.provider(tt.hostname); got != tt.provider {
			t.Errorf("%s %s: provider %q, want %q", tt.dmi, tt.hostname, got, tt.provider)
		}
		if got := board.virtual(); got != tt.virtual {
			t.Errorf("%s: virtual %v, want %v", tt.dmi, got, tt.virtual)
		}
	}
}

func TestEC2UUID(t *testing.T) {
	for uuid, want := range map[string]bool{
		"ec2e1916-9099-7caf-fd21-012345abcdef": true,
		"45E12AEC-DCD1-B213-94ED-012345ABCDEF": true, // little-endian form
		"4c4c4544-0042-3510-8051-b7c04f4d3732": false,
		"":                                     false,
	} {
		if got := ec2UUID(uuid); got != want {
			t.Errorf("%q: got %v, want %v", uuid, got, want)
		}
	}
}

// metadataServer answers like the provider services: requests without the
// right headers get 401.
func metadataServer(t *testing.T) string {
	mux := http.NewServeMux()
	need := func(header, value, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(header) != value {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Write([]byte(body))
		}
	}
	mux.HandleFunc("PUT /latest/api/token", need("X-Aws-Ec2-Metadata-Token-Ttl-Seconds", "60", "aws-token"))
	mux.HandleFunc("GET /latest/meta-data/placement/region", need("X-Aws-Ec2-Metadata-Token", "aws-token", "eu-central-1"))
	mux.HandleFunc("PUT /v1/token", need("Metadata-Token-Expiry-Seconds", "60", "linode-token\n"))
	mux.HandleFunc("GET /v1/instance", need("Metadata-Token", "linode-token",
		"backups.enabled: false\nid: 123\nlabel: ubuntu-us-iad-001\nregion: us-iad\nspecs.vcpus: 1\n"))
	mux.HandleFunc("GET /computeMetadata/v1/instance/zone", need("Metadata-Flavor", "Google", "projects/123456/zones/europe-west1-b"))
	mux.HandleFunc("GET /metadata/instance/compute/location", need("Metadata", "true", "westeurope"))
	mux.HandleFunc("GET /hetzner/v1/metadata/availability-zone", need("", "", "fsn1-dc14\n"))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRegion(t *testing.T) {
	base := metadataServer(t)
	tests := map[string]string{
		providerEC2:     "eu-central-1",
		providerLinode:  "us-iad",
		providerGCE:     "europe-west1-b",
		providerAzure:   "westeurope",
		providerHetzner: "fsn1-dc14",
		providerVultr:   "", // 404 from this server
		providerContabo: "", // no metadata service
	}
	for provider, want := range tests {
		if got := region(context.Background(), base, provider); got != want {
			t.Errorf("%s: got %q, want %q", provider, got, want)
		}
	}
}
