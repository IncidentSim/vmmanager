package nextermaccess

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMonitoringReadbackOnlyAcceptsBooleanTrue(t *testing.T) {
	for _, raw := range []string{`true`, `false`, `1`, `"true"`, `null`} {
		var wire map[string]any
		if err := json.Unmarshal([]byte(`{"id":1,"config":{"protocol":"ssh","monitoringEnabled":`+raw+`}}`), &wire); err != nil {
			t.Fatal(err)
		}
		en, ok := entryFrom(wire)
		if !ok || en.MonitoringEnabled != (raw == "true") {
			t.Fatalf("incorrect monitoring flag for %s", raw)
		}
	}
}

func TestMonitoringPatchPreservesLocalConfigWithoutMaterializingInheritance(t *testing.T) {
	local := map[string]any{"protocol": "ssh", "ip": "localhost", "port": 22, "notes": "local note"}
	patches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/entries/42" {
			t.Errorf("wrong target %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if r.Method == "PATCH" {
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			cfg, _ := body["config"].(map[string]any)
			if len(body) != 1 || len(cfg) != 5 || cfg["protocol"] != "ssh" || cfg["monitoringEnabled"] != true || cfg["notes"] != "local note" || cfg["inheritedSetting"] != nil {
				t.Errorf("unsafe config patch: %v", body)
			}
			local = cfg
			patches++
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		effective := map[string]any{}
		for k, v := range local {
			effective[k] = v
		}
		effective["inheritedSetting"] = "do not copy"
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "config": effective, "localConfig": local, "identities": []int64{7}})
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	en, err := client.GetEntry(context.Background(), "session", 42)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.enableMonitoring(context.Background(), "session", en)
	if err != nil || !got.MonitoringEnabled || patches != 1 {
		t.Fatalf("repair readback: %v", err)
	}
}

func TestProvisionResumesLegacyEntryAndRepairsMonitoring(t *testing.T) {
	v, rec := linkSetup(t)
	v.nx.Entries[rec.UserID][rec.EntryID]["config"].(map[string]any)["monitoringEnabled"] = false
	rec.Stage = StagePlanned // interrupted provisioning with all object IDs recorded
	if err := v.store.Put(rec); err != nil {
		t.Fatal(err)
	}
	got, err := v.m.Ensure(context.Background(), 7, "vm-a")
	if err != nil || got.Status != Ready || v.nx.MonitoringUpdates != 1 {
		t.Fatalf("resume failed: %v %+v", err, got)
	}
	if v.nx.NonAdmin() != 1 || len(v.nx.Entries[rec.UserID]) != 1 || len(v.nx.Identities[rec.UserID]) != 1 || len(v.nx.Sessions) != 0 || v.api.keyCalls != 1 {
		t.Fatal("resume duplicated objects, key retrieval or sessions")
	}
}

func TestProvisionExplicitlyEnablesMonitoring(t *testing.T) {
	v, rec := linkSetup(t)
	cfg := v.nx.Entries[rec.UserID][rec.EntryID]["config"].(map[string]any)
	if cfg["monitoringEnabled"] != true || v.nx.MonitoringUpdates != 0 {
		t.Fatal("new SSH entry must explicitly enable monitoring at creation")
	}
}

func TestPrepareLinkRepairsMonitoringWithoutDuplicatingObjectsOrSessions(t *testing.T) {
	for _, existingSession := range []bool{false, true} {
		t.Run(map[bool]string{false: "first link", true: "reused link"}[existingSession], func(t *testing.T) {
			v, rec := linkSetup(t)
			ctx := context.Background()
			var token string
			if existingSession {
				link, err := v.m.PrepareLink(ctx, 7, "vm-a")
				if err != nil {
					t.Fatal(err)
				}
				token = linkToken(t, link)
			}
			real := v.nx.Login(rec.UserID)
			en := v.nx.Entries[rec.UserID][rec.EntryID]
			cfg := en["config"].(map[string]any)
			delete(cfg, "monitoringEnabled") // legacy ready binding
			cfg["notes"] = "retain personal configuration"
			v.nx.Entries[rec.UserID][999] = map[string]any{"config": map[string]any{"monitoringEnabled": false}}
			beforeKeys := v.api.keyCalls
			for i := 0; i < 3; i++ {
				link, err := v.m.PrepareLink(ctx, 7, "vm-a")
				if err != nil {
					t.Fatal(err)
				}
				if token == "" {
					token = linkToken(t, link)
				}
				if linkToken(t, link) != token {
					t.Fatal("link session changed")
				}
			}
			cfg = en["config"].(map[string]any)
			if cfg["monitoringEnabled"] != true || cfg["notes"] != "retain personal configuration" || cfg["ip"] != "185.70.184.64" || cfg["port"] != float64(22001) {
				t.Fatal("monitoring not enabled or config overwritten")
			}
			if v.nx.MonitoringUpdates != 1 || v.nx.NonAdmin() != 1 || len(v.nx.Identities[rec.UserID]) != 1 || len(v.nx.Entries[rec.UserID]) != 2 || v.api.keyCalls != beforeKeys {
				t.Fatal("repair was not idempotent or duplicated managed objects")
			}
			if v.nx.Entries[rec.UserID][999]["config"].(map[string]any)["monitoringEnabled"] != false {
				t.Fatal("unrecorded entry changed")
			}
			if len(v.nx.Sessions) != 2 || v.nx.Sessions[real] != rec.UserID || v.nx.Sessions[token] != rec.UserID || v.nx.GlobalLogout {
				t.Fatal("sessions leaked or real session destroyed")
			}
			got, _ := v.store.Get("vm-a")
			if got.Stage != StageReady || got.UserID != rec.UserID || got.EntryID != rec.EntryID || got.IdentityID != rec.IdentityID || got.Username != rec.Username || got.Password != rec.Password {
				t.Fatal("ready binding replaced")
			}
		})
	}
}

func TestMonitoringRepairFailsClosedAndRetriesSameSession(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(map[bool]string{false: "domain failure", true: "successful no-op"}[ignore], func(t *testing.T) {
			v, rec := linkSetup(t)
			ctx := context.Background()
			link, err := v.m.PrepareLink(ctx, 7, "vm-a")
			if err != nil {
				t.Fatal(err)
			}
			token := linkToken(t, link)
			v.nx.Entries[rec.UserID][rec.EntryID]["config"].(map[string]any)["monitoringEnabled"] = false
			v.nx.FailMonitoring = !ignore
			v.nx.IgnoreMonitoring = ignore
			if _, err := v.m.PrepareLink(ctx, 7, "vm-a"); err == nil {
				t.Fatal("unverified repair accepted")
			}
			if len(v.nx.Sessions) != 1 || v.nx.Sessions[token] != rec.UserID {
				t.Fatal("failure minted or destroyed sessions")
			}
			v.nx.FailMonitoring, v.nx.IgnoreMonitoring = false, false
			link, err = v.m.PrepareLink(ctx, 7, "vm-a")
			if err != nil || linkToken(t, link) != token {
				t.Fatalf("retry did not reuse session: %v", err)
			}
		})
	}
}

func TestMonitoringRepairRequiresOwnershipAndExactAccountEntry(t *testing.T) {
	cases := map[string]func(*env, Record){
		"wrong owner":    func(v *env, r Record) {},
		"admin account":  func(v *env, r Record) { v.nx.AdminImpers = true },
		"wrong entry id": func(v *env, r Record) { v.nx.Entries[r.UserID][r.EntryID]["id"] = r.EntryID + 1 },
		"wrong protocol": func(v *env, r Record) {
			v.nx.Entries[r.UserID][r.EntryID]["config"].(map[string]any)["protocol"] = "rdp"
		},
		"wrong identity": func(v *env, r Record) { v.nx.Entries[r.UserID][r.EntryID]["identities"] = []int64{999} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			v, rec := linkSetup(t)
			v.nx.Entries[rec.UserID][rec.EntryID]["config"].(map[string]any)["monitoringEnabled"] = false
			mutate(v, rec)
			owner := int64(7)
			if name == "wrong owner" {
				owner = 8
			}
			if _, err := v.m.PrepareLink(context.Background(), owner, "vm-a"); err == nil {
				t.Fatal("unsafe repair accepted")
			}
			if v.nx.MonitoringUpdates != 0 || len(v.nx.Sessions) != 0 {
				t.Fatal("unsafe write or leaked session")
			}
		})
	}
}
