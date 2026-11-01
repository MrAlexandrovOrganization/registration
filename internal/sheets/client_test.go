package sheets

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"registration.local/backend/internal/service"
)

func TestReplaceAtomicLiteralSnapshot(t *testing.T) {
	var batches []map[string]json.RawMessage
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method == http.MethodGet {
			if !strings.Contains(r.URL.RawQuery, "gridProperties") {
				t.Error("must request only metadata")
			}
			w.Write([]byte(`{"sheets":[{"properties":{"sheetId":17,"gridProperties":{"rowCount":1000,"columnCount":26}}}]}`))
			return
		}
		if r.URL.Path != "/book:batchUpdate" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
		var body struct {
			Requests []map[string]json.RawMessage `json:"requests"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		batches = body.Requests
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := &Client{http: server.Client(), endpoint: server.URL + "/", SpreadsheetID: "book", SheetID: 17}
	table := service.ExportTable{Rows: [][]string{{"phone", "answer"}, {"07001234567", "=IMPORTXML(\"x\")"}}, Widths: []float64{22, 42}}
	if err := client.Replace(t.Context(), table); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(batches) != 8 {
		t.Fatalf("expected metadata + atomic batch, got %d calls, %d requests", calls, len(batches))
	}
	var clear struct {
		Range  map[string]int `json:"range"`
		Fields string         `json:"fields"`
	}
	if err := json.Unmarshal(batches[1]["updateCells"], &clear); err != nil {
		t.Fatal(err)
	}
	if len(clear.Range) != 1 || clear.Range["sheetId"] != 17 || clear.Fields != "userEnteredValue" {
		t.Fatalf("old rows/columns must all be cleared: %+v", clear)
	}
	var update struct {
		Rows []row `json:"rows"`
	}
	if err := json.Unmarshal(batches[2]["updateCells"], &update); err != nil {
		t.Fatal(err)
	}
	if update.Rows[1].Values[0].Value.Text != "07001234567" || update.Rows[1].Values[1].Value.Text != table.Rows[1][1] {
		t.Fatal("data not preserved as literal text")
	}
	if strings.Contains(string(batches[2]["updateCells"]), "formulaValue") {
		t.Fatal("formula injection")
	}
	// Empty DB must still replace old contents with just the header.
	table.Rows = table.Rows[:1]
	if err := client.Replace(t.Context(), table); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(batches[2]["updateCells"], &update); err != nil {
		t.Fatal(err)
	}
	if len(update.Rows) != 1 {
		t.Fatal("empty export did not clear participants")
	}
}

func TestSheetsErrorsAreRedacted(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				w.Write([]byte(`secret-token personal-data`))
			}))
			defer server.Close()
			client := &Client{http: server.Client(), endpoint: server.URL + "/", SpreadsheetID: "private-book"}
			err := client.Replace(t.Context(), service.ExportTable{Rows: [][]string{{"name"}}, Widths: []float64{22}})
			if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private-book") {
				t.Fatalf("unsafe error: %v", err)
			}
		})
	}
}

func TestMissingSheetNeverWrites(t *testing.T) {
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
		}
		w.Write([]byte(`{"sheets":[]}`))
	}))
	defer server.Close()
	client := &Client{http: server.Client(), endpoint: server.URL + "/", SpreadsheetID: "book"}
	if err := client.Replace(t.Context(), service.ExportTable{Rows: [][]string{{"name"}}, Widths: []float64{22}}); err == nil || writes != 0 {
		t.Fatal("missing sheet must fail before writing")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := client.Replace(ctx, service.ExportTable{Rows: [][]string{{"name"}}, Widths: []float64{22}}); err == nil {
		t.Fatal("cancellation ignored")
	}
}

func TestServiceAccountAuthentication(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	tokens := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens++
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("assertion") == "" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
				t.Error("missing service account JWT")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"synthetic-fixture-token","token_type":"Bearer","expires_in":3600}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-fixture-token" {
			t.Error("missing Google authorization")
		}
		w.Write([]byte(`{}`))
	}))
	defer server.Close()
	credentials, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "fixture@example.invalid", "private_key": string(private), "token_uri": server.URL + "/token"})
	path := filepath.Join(t.TempDir(), "key.json")
	if err = os.WriteFile(path, credentials, 0600); err != nil {
		t.Fatal(err)
	}
	client, err := New(t.Context(), path, "book", 0)
	if err != nil {
		t.Fatal(err)
	}
	client.endpoint = server.URL + "/"
	for range 2 {
		if err = client.request(t.Context(), http.MethodGet, "", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if tokens != 1 {
		t.Fatal("OAuth token not reused")
	}
}
