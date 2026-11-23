package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vt100io/smed/watch/machine"
)

func testHandler() http.Handler {
	return handler(machine.Reader{
		Proc: "../../watch/machine/testdata/proc", Etc: "../../watch/machine/testdata/etc",
		Statfs: func(string) (uint64, uint64, uint64, error) { return 100, 10, 90, nil },
		Now:    time.Now,
	})
}

func TestGetAnswersWithReport(t *testing.T) {
	rec := httptest.NewRecorder()
	testHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
	var rep machine.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Version != machine.Version || rep.Memory.TotalBytes == 0 {
		t.Errorf("report %+v", rep)
	}
}

// Read-only by construction: anything but a question is refused.
func TestOnlyQuestions(t *testing.T) {
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		rec := httptest.NewRecorder()
		testHandler().ServeHTTP(rec, httptest.NewRequest(method, "/", strings.NewReader("x")))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d", method, rec.Code)
		}
	}
	rec := httptest.NewRecorder()
	testHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/etc/passwd", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /etc/passwd: status %d", rec.Code)
	}
}
