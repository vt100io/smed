package hetzner

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerTypes(t *testing.T) {
	const (
		gone  = `{"name":"old1","architecture":"x86","cpu_type":"shared","cores":1,"memory":2.0,"disk":20,"prices":[{"location":"hel1","price_monthly":{"net":"9.0","gross":"11.25"}}],"locations":[{"name":"hel1","available":true,"deprecation":{"unavailable_after":"2027-02-12T00:00:00Z"}}]}`
		full  = `{"name":"big1","architecture":"x86","cpu_type":"dedicated","cores":4,"memory":16.0,"disk":160,"prices":[{"location":"fsn1","price_monthly":{"net":"1.0","gross":"1.25"}},{"location":"hel1","price_monthly":{"net":"20.0","gross":"25.0"}}],"locations":[{"name":"fsn1","available":true,"deprecation":null},{"name":"hel1","available":false,"deprecation":null}]}`
		cheap = `{"name":"cx1","architecture":"x86","cpu_type":"shared","cores":2,"memory":0.5,"disk":40,"prices":[{"location":"hel1","price_monthly":{"net":"5.4900000000","gross":"6.8625000000"}}],"locations":[{"name":"hel1","available":true,"deprecation":null}]}`
		other = `{"name":"us1","prices":[{"location":"ash","price_monthly":{"net":"1.0","gross":"1.0"}}],"locations":[{"name":"ash","available":true}]}`
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("%s %s: listing must only read", r.Method, r.URL)
		}
		body := ""
		switch q := r.URL.Query(); {
		case r.URL.Path == "/locations" && q.Get("name") == "hel1":
			body = `{"locations":[{"name":"hel1"}]}`
		case r.URL.Path == "/locations":
			body = `{"locations":[]}`
		case q.Get("page") == "1":
			body = `{"server_types":[` + full + `,` + other + `],"meta":{"pagination":{"next_page":2}}}`
		default:
			body = `{"server_types":[` + gone + `,` + cheap + `],"meta":{"pagination":{"next_page":null}}}`
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	a := Account{BaseURL: srv.URL, Token: func(context.Context) (string, error) { return "t", nil }}

	types, err := a.ServerTypes(t.Context(), "hel1")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ty := range types {
		names = append(names, ty.Name)
	}
	if got := strings.Join(names, " "); got != "cx1 old1 big1" {
		t.Errorf("types = %s, want cheapest in hel1 first, across pages", got)
	}
	out := types.String()
	t.Log("\n" + out)
	for _, want := range []string{"0.5 GB", "€5.49", "€6.86", "available, until 2027-02-12", "unavailable"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q", want)
		}
	}
	if _, err := a.ServerTypes(t.Context(), "nope"); err == nil || !strings.Contains(err.Error(), `no location "nope"`) {
		t.Errorf("unknown location: %v", err)
	}
}
