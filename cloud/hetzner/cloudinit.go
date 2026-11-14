package hetzner

import (
	"encoding/json"
	"strings"

	"github.com/vt100io/smed/shellquote"
)

// tailscaleInstall is Tailscale's own install script, which picks the right
// package repository for the image.
const tailscaleInstall = "curl -fsSL https://tailscale.com/install.sh | sh"

// cloudInit is the server's first-boot script. It always turns off password
// logins for SSH (Hetzner emails a root password when a server has no SSH
// key). With an auth key, it also installs Tailscale and joins the tailnet
// under hostname, with Tailscale SSH and the given tags.
//
// The script, auth key included, stays readable to root on the server
// afterwards, so the key should be single use and short-lived.
func cloudInit(hostname, authKey string, tags []string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\n")
	b.WriteString("ssh_pwauth: false\n")
	if authKey == "" {
		return b.String()
	}
	up := "tailscale up --ssh --auth-key=" + shellquote.Quote(authKey) + " --hostname=" + shellquote.Quote(hostname)
	if len(tags) > 0 {
		up += " --advertise-tags=" + shellquote.Quote(strings.Join(tags, ","))
	}
	b.WriteString("runcmd:\n")
	b.WriteString("  - " + yamlString(tailscaleInstall) + "\n")
	b.WriteString("  - " + yamlString(up) + "\n")
	return b.String()
}

// yamlString quotes s for YAML. A JSON string is a valid YAML
// double-quoted scalar.
func yamlString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
