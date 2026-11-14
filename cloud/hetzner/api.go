package hetzner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// client is the small part of the Hetzner Cloud API this package needs:
// servers, firewalls and actions. Plain HTTP and JSON, no SDK.
type client struct {
	base  string
	http  *http.Client
	token string
}

type apiServer struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	Status     string            `json:"status"`
	Labels     map[string]string `json:"labels"`
	ServerType struct {
		Name string `json:"name"`
	} `json:"server_type"`
	Location struct {
		Name string `json:"name"`
	} `json:"location"`
	// Datacenter is how older API responses carried the location.
	Datacenter struct {
		Location struct {
			Name string `json:"name"`
		} `json:"location"`
	} `json:"datacenter"`
	Image *struct {
		Name string `json:"name"`
	} `json:"image"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

// location is where the server is, from whichever field the API used.
func (s apiServer) location() string {
	if s.Location.Name != "" {
		return s.Location.Name
	}
	return s.Datacenter.Location.Name
}

type apiRule struct {
	Direction string   `json:"direction"`
	Protocol  string   `json:"protocol"`
	Port      string   `json:"port,omitempty"`
	SourceIPs []string `json:"source_ips"`
}

type apiFirewall struct {
	ID     int64             `json:"id"`
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Rules  []apiRule         `json:"rules"`
}

type apiAction struct {
	ID     int64  `json:"id"`
	Status string `json:"status"` // running, success or error
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type apiFirewallRef struct {
	Firewall int64 `json:"firewall"`
}

type apiApplyTo struct {
	Type   string `json:"type"`
	Server struct {
		ID int64 `json:"id"`
	} `json:"server"`
}

// do sends one request. A non-2xx response becomes an error carrying
// Hetzner's own message.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("hetzner: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("hetzner: %s %s: reading response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return fmt.Errorf("hetzner: %s %s: %s (%s)", method, path, e.Error.Message, e.Error.Code)
		}
		return fmt.Errorf("hetzner: %s %s: %s", method, path, resp.Status)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("hetzner: %s %s: decoding response: %w", method, path, err)
		}
	}
	return nil
}

type pagination struct {
	Meta struct {
		Pagination struct {
			NextPage *int `json:"next_page"`
		} `json:"pagination"`
	} `json:"meta"`
}

// listServers returns every server matching query (label_selector or name),
// across all pages.
func (c *client) listServers(ctx context.Context, query url.Values) ([]apiServer, error) {
	var all []apiServer
	for page := 1; ; {
		q := cloneWith(query, "page", strconv.Itoa(page))
		var resp struct {
			Servers []apiServer `json:"servers"`
			pagination
		}
		if err := c.do(ctx, http.MethodGet, "/servers?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Servers...)
		if resp.Meta.Pagination.NextPage == nil {
			return all, nil
		}
		page = *resp.Meta.Pagination.NextPage
	}
}

func (c *client) listFirewalls(ctx context.Context, query url.Values) ([]apiFirewall, error) {
	var all []apiFirewall
	for page := 1; ; {
		q := cloneWith(query, "page", strconv.Itoa(page))
		var resp struct {
			Firewalls []apiFirewall `json:"firewalls"`
			pagination
		}
		if err := c.do(ctx, http.MethodGet, "/firewalls?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Firewalls...)
		if resp.Meta.Pagination.NextPage == nil {
			return all, nil
		}
		page = *resp.Meta.Pagination.NextPage
	}
}

// wait polls an action until it succeeds, fails, or ctx ends.
func (c *client) wait(ctx context.Context, id int64, every time.Duration) error {
	for {
		var resp struct {
			Action apiAction `json:"action"`
		}
		if err := c.do(ctx, http.MethodGet, "/actions/"+strconv.FormatInt(id, 10), nil, &resp); err != nil {
			return err
		}
		switch resp.Action.Status {
		case "success":
			return nil
		case "error":
			if e := resp.Action.Error; e != nil {
				return fmt.Errorf("hetzner: action %d failed: %s (%s)", id, e.Message, e.Code)
			}
			return fmt.Errorf("hetzner: action %d failed", id)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("hetzner: waiting for action %d: %w", id, ctx.Err())
		case <-time.After(every):
		}
	}
}

func (c *client) waitAll(ctx context.Context, actions []apiAction, every time.Duration) error {
	for _, a := range actions {
		if err := c.wait(ctx, a.ID, every); err != nil {
			return err
		}
	}
	return nil
}

func cloneWith(v url.Values, key, value string) url.Values {
	out := url.Values{}
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	out.Set(key, value)
	out.Set("per_page", "50")
	return out
}

type apiServerType struct {
	Name         string `json:"name"`
	Architecture string `json:"architecture"`
	Deprecated   bool   `json:"deprecated"`
	Prices       []struct {
		Location     string `json:"location"`
		PriceMonthly struct {
			Gross string `json:"gross"`
		} `json:"price_monthly"`
	} `json:"prices"`
}

type apiImage struct {
	Name         string  `json:"name"`
	Architecture string  `json:"architecture"`
	Deprecated   *string `json:"deprecated"` // when it was deprecated, or null
}

func (c *client) serverType(ctx context.Context, name string) (*apiServerType, error) {
	var resp struct {
		ServerTypes []apiServerType `json:"server_types"`
	}
	if err := c.do(ctx, http.MethodGet, "/server_types?"+url.Values{"name": {name}}.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	if len(resp.ServerTypes) == 0 {
		return nil, nil
	}
	return &resp.ServerTypes[0], nil
}

func (c *client) locationExists(ctx context.Context, name string) (bool, error) {
	var resp struct {
		Locations []struct{ Name string } `json:"locations"`
	}
	if err := c.do(ctx, http.MethodGet, "/locations?"+url.Values{"name": {name}}.Encode(), nil, &resp); err != nil {
		return false, err
	}
	return len(resp.Locations) > 0, nil
}

// systemImage returns the system image called name for an architecture.
func (c *client) systemImage(ctx context.Context, name, arch string) (*apiImage, error) {
	q := url.Values{"name": {name}, "type": {"system"}, "architecture": {arch}}
	var resp struct {
		Images []apiImage `json:"images"`
	}
	if err := c.do(ctx, http.MethodGet, "/images?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	if len(resp.Images) == 0 {
		return nil, nil
	}
	return &resp.Images[0], nil
}

type apiSSHKey struct {
	Name      string `json:"name"`
	PublicKey string `json:"public_key"`
}

func (c *client) sshKey(ctx context.Context, name string) (*apiSSHKey, error) {
	var resp struct {
		SSHKeys []apiSSHKey `json:"ssh_keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/ssh_keys?"+url.Values{"name": {name}}.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	if len(resp.SSHKeys) == 0 {
		return nil, nil
	}
	return &resp.SSHKeys[0], nil
}
