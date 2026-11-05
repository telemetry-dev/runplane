package host

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Provider names that Info.Provider can hold.
const (
	providerHetzner      = "Hetzner"
	providerDigitalOcean = "DigitalOcean"
	providerOVH          = "OVHcloud"
	providerContabo      = "Contabo"
	providerLinode       = "Akamai/Linode"
	providerVultr        = "Vultr"
	providerEC2          = "Amazon EC2"
	providerGCE          = "Google Compute Engine"
	providerAzure        = "Microsoft Azure"
	providerNetcup       = "netcup"
)

const metadataURL = "http://169.254.169.254"

// Azure writes this asset tag on all of its VMs; plain Hyper-V does not.
const azureAssetTag = "7783-7084-3265-9085-8269-3286-77"

// Default host names of providers whose DMI data names only the hypervisor.
var (
	contaboHost = regexp.MustCompile(`^vm[id]\d{5,}$|\.contaboserver\.net$`)
	netcupHost  = regexp.MustCompile(`^v\d{10,}\.[a-z]+srv\.de$`)
	ovhHost     = regexp.MustCompile(`\.ovh\.net$|^ns\d+\.ip-\d+-\d+-\d+\.[a-z]+$`)
)

// dmi holds the SMBIOS strings from /sys/class/dmi/id. Only root can read
// product_serial and product_uuid; they stay empty otherwise.
type dmi struct {
	sysVendor, productName, productSerial, productUUID string
	biosVendor, biosVersion                            string
	boardVendor, chassisVendor, chassisAssetTag        string
}

func readDMI(fsys fs.FS) dmi {
	read := func(name string) string {
		b, _ := fs.ReadFile(fsys, name)
		return strings.TrimSpace(string(b))
	}
	return dmi{
		sysVendor:       read("sys_vendor"),
		productName:     read("product_name"),
		productSerial:   read("product_serial"),
		productUUID:     read("product_uuid"),
		biosVendor:      read("bios_vendor"),
		biosVersion:     read("bios_version"),
		boardVendor:     read("board_vendor"),
		chassisVendor:   read("chassis_vendor"),
		chassisAssetTag: read("chassis_asset_tag"),
	}
}

// names returns the vendor and product names in lower case.
func (d dmi) names() string {
	return strings.ToLower(strings.Join([]string{d.sysVendor, d.productName, d.biosVendor, d.boardVendor, d.chassisVendor}, " "))
}

// provider guesses the hosting provider. The DMI rules follow cloud-init's
// ds-identify. It returns "" for unknown providers and most bare metal.
func (d dmi) provider(hostname string) string {
	text := d.names()
	hostname = strings.ToLower(hostname)
	switch {
	case d.chassisAssetTag == azureAssetTag:
		return providerAzure
	case d.sysVendor == "Amazon EC2" || d.biosVendor == "Amazon EC2" ||
		strings.Contains(strings.ToLower(d.biosVersion), "amazon") || ec2UUID(d.productUUID):
		return providerEC2
	case d.productName == "Google Compute Engine" || d.sysVendor == "Google" || strings.HasPrefix(d.productSerial, "GoogleCloud-"):
		return providerGCE
	case d.sysVendor == "Hetzner":
		return providerHetzner
	case d.sysVendor == "DigitalOcean":
		return providerDigitalOcean
	case d.sysVendor == "Vultr":
		return providerVultr
	case d.sysVendor == "Linode" || d.sysVendor == "Akamai":
		return providerLinode
	case strings.Contains(text, "ovh") || ovhHost.MatchString(hostname):
		return providerOVH
	case strings.Contains(text, "netcup") || netcupHost.MatchString(hostname):
		return providerNetcup
	case strings.Contains(text, "contabo") || contaboHost.MatchString(hostname):
		return providerContabo
	}
	return ""
}

// ec2UUID reports if a product UUID starts with "ec2", also in the
// little-endian form that some instance types show.
func ec2UUID(uuid string) bool {
	u := strings.ToLower(uuid)
	return strings.HasPrefix(u, "ec2") || len(u) >= 8 && u[4] == '2' && u[6:8] == "ec"
}

// virtual reports if the DMI data names a hypervisor.
func (d dmi) virtual() bool {
	text := d.names()
	return slices.ContainsFunc([]string{"qemu", "kvm", "virtual", "vmware", "xen", "bochs", "bhyve", "openstack", "vserver", "droplet"},
		func(word string) bool { return strings.Contains(text, word) })
}

// region asks the metadata service of the provider for the region or zone.
// It returns "" for providers without a simple endpoint and on any error.
func region(ctx context.Context, base, provider string) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// The zero Transport uses no proxy; a proxy cannot reach link-local addresses.
	client := &http.Client{Transport: &http.Transport{}}
	// ask returns the trimmed body of a 200 response, or "".
	ask := func(method, path string, h http.Header) string {
		req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
		if err != nil {
			return ""
		}
		if h != nil {
			req.Header = h
		}
		resp, err := client.Do(req)
		if err != nil {
			return ""
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if err != nil || resp.StatusCode != http.StatusOK {
			return ""
		}
		return strings.TrimSpace(string(body))
	}
	get := func(path string, h http.Header) string { return ask(http.MethodGet, path, h) }
	switch provider {
	case providerHetzner:
		return get("/hetzner/v1/metadata/availability-zone", nil)
	case providerDigitalOcean:
		return get("/metadata/v1/region", nil)
	case providerVultr:
		return get("/v1/region/regioncode", nil)
	case providerGCE:
		zone := get("/computeMetadata/v1/instance/zone", http.Header{"Metadata-Flavor": {"Google"}})
		return path.Base(zone) // projects/<n>/zones/<zone>
	case providerAzure:
		return get("/metadata/instance/compute/location?api-version=2021-02-01&format=text", http.Header{"Metadata": {"true"}})
	case providerEC2:
		token := ask(http.MethodPut, "/latest/api/token", http.Header{"X-Aws-Ec2-Metadata-Token-Ttl-Seconds": {"60"}})
		if token == "" {
			return ""
		}
		return get("/latest/meta-data/placement/region", http.Header{"X-Aws-Ec2-Metadata-Token": {token}})
	case providerLinode:
		token := ask(http.MethodPut, "/v1/token", http.Header{"Metadata-Token-Expiry-Seconds": {"60"}})
		if token == "" {
			return ""
		}
		for line := range strings.Lines(get("/v1/instance", http.Header{"Metadata-Token": {token}})) {
			if v, ok := strings.CutPrefix(line, "region:"); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}
