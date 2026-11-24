package hetzner

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
)

// ServerType is a kind of server Hetzner offers in one location.
type ServerType struct {
	Name         string
	Architecture string // "x86" or "arm"
	CPU          string // "shared" or "dedicated"
	Cores        int
	MemoryGB     float64
	DiskGB       int
	Net, Gross   float64 // euros per month, without and with VAT; a public IPv4 address costs extra
	Available    bool    // false: Hetzner has none to sell in the location right now
	Deprecated   string  // the date it can't be ordered after, or ""
}

// ServerTypes is a list of server types that prints as a table.
type ServerTypes []ServerType

// ServerTypes asks Hetzner what it offers in location ("hel1"), cheapest
// first. It changes nothing; a read-only token is enough.
func (a Account) ServerTypes(ctx context.Context, location string) (ServerTypes, error) {
	c, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	ok, err := c.locationExists(ctx, location)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("hetzner: there is no location %q", location)
	}
	offers, err := c.listServerTypes(ctx)
	if err != nil {
		return nil, err
	}
	var types ServerTypes
	for _, o := range offers {
		t := ServerType{Name: o.Name, Architecture: o.Architecture, CPU: o.CPUType, Cores: o.Cores, MemoryGB: o.Memory, DiskGB: o.Disk}
		offered := false
		for _, l := range o.Locations {
			if l.Name != location {
				continue
			}
			offered, t.Available = true, l.Available
			if l.Deprecation != nil {
				t.Deprecated = l.Deprecation.date()
			}
		}
		priced := false
		for _, p := range o.Prices {
			if p.Location != location {
				continue
			}
			priced = true
			if t.Net, err = strconv.ParseFloat(p.PriceMonthly.Net, 64); err != nil {
				return nil, fmt.Errorf("hetzner: price of %s: %w", o.Name, err)
			}
			if t.Gross, err = strconv.ParseFloat(p.PriceMonthly.Gross, 64); err != nil {
				return nil, fmt.Errorf("hetzner: price of %s: %w", o.Name, err)
			}
		}
		if offered && priced {
			types = append(types, t)
		}
	}
	slices.SortFunc(types, func(a, b ServerType) int {
		return cmp.Or(cmp.Compare(a.Net, b.Net), strings.Compare(a.Name, b.Name))
	})
	return types, nil
}

func (ts ServerTypes) String() string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tARCH\tCPU\tCORES\tRAM\tDISK\tNET/MONTH\tWITH VAT\tSTATUS")
	for _, t := range ts {
		status := "available"
		if !t.Available {
			status = "unavailable"
		}
		if t.Deprecated != "" {
			status += ", until " + t.Deprecated
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%g GB\t%d GB\t€%.2f\t€%.2f\t%s\n",
			t.Name, t.Architecture, t.CPU, t.Cores, t.MemoryGB, t.DiskGB, t.Net, t.Gross, status)
	}
	w.Flush()
	return b.String()
}

// listServerTypes returns every server type, across all pages.
func (c *client) listServerTypes(ctx context.Context) ([]apiServerType, error) {
	var all []apiServerType
	for page := 1; ; {
		q := cloneWith(url.Values{}, "page", strconv.Itoa(page))
		var resp struct {
			ServerTypes []apiServerType `json:"server_types"`
			pagination
		}
		if err := c.do(ctx, http.MethodGet, "/server_types?"+q.Encode(), nil, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.ServerTypes...)
		if resp.Meta.Pagination.NextPage == nil {
			return all, nil
		}
		page = *resp.Meta.Pagination.NextPage
	}
}
