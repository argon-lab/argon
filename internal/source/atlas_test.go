package source

import (
	"strings"
	"testing"
)

const atlasHosts = "ac-a-shard-00-00.q4bf3vu.mongodb.net:27017,ac-a-shard-00-01.q4bf3vu.mongodb.net:27017"

var reportedAtlas = []string{"ac-a-shard-00-00.q4bf3vu.mongodb.net:27017", "ac-a-shard-00-01.q4bf3vu.mongodb.net:27017", "ac-a-shard-00-02.q4bf3vu.mongodb.net:27017"}

// Every way a connection can reach Atlas hosts, and the ones that weaken or
// replace TLS verification. Only the first two are verified.
func TestVerifiedAtlasConnection(t *testing.T) {
	cases := []struct {
		name     string
		uri      string
		reported []string
		want     bool
	}{
		{"tls on, system roots", "mongodb://u:p@" + atlasHosts + "/?tls=true&replicaSet=atlas-x-shard-0", reportedAtlas, true},
		{"ssl alias on", "mongodb://" + atlasHosts + "/?ssl=true", reportedAtlas, true},
		{"tls off", "mongodb://" + atlasHosts + "/?replicaSet=atlas-x-shard-0", reportedAtlas, false},
		{"tls explicitly false", "mongodb://" + atlasHosts + "/?tls=false", reportedAtlas, false},
		{"tlsInsecure", "mongodb://" + atlasHosts + "/?tls=true&tlsInsecure=true", reportedAtlas, false},
		{"tlsInsecure mixed case", "mongodb://" + atlasHosts + "/?tls=true&TLSINSECURE=true", reportedAtlas, false},
		{"tlsAllowInvalidCertificates", "mongodb://" + atlasHosts + "/?tls=true&tlsAllowInvalidCertificates=true", reportedAtlas, false},
		{"tlsAllowInvalidHostnames", "mongodb://" + atlasHosts + "/?tls=true&tlsAllowInvalidHostnames=true", reportedAtlas, false},
		{"custom CA file", "mongodb://" + atlasHosts + "/?tls=true&tlsCAFile=/tmp/ca.pem", reportedAtlas, false},
		{"seed host outside Atlas", "mongodb://db.example.com:27017/?tls=true", reportedAtlas, false},
		{"one seed host outside Atlas", "mongodb://" + atlasHosts + ",db.example.com:27017/?tls=true", reportedAtlas, false},
		{"suffix without its dot", "mongodb://evilmongodb.net:27017/?tls=true", []string{"evilmongodb.net:27017"}, false},
		{"Atlas name inside another domain", "mongodb://x.mongodb.net.example.com:27017/?tls=true", []string{"x.mongodb.net.example.com:27017"}, false},
		{"the bare domain", "mongodb://mongodb.net:27017/?tls=true", []string{"mongodb.net:27017"}, false},
		{"IP literal", "mongodb://10.0.0.1:27017/?tls=true", []string{"10.0.0.1:27017"}, false},
		{"member reported outside Atlas", "mongodb://" + atlasHosts + "/?tls=true", append(append([]string{}, reportedAtlas...), "attacker.example.com:27017"), false},
		{"no members reported", "mongodb://" + atlasHosts + "/?tls=true", nil, false},
		{"unparseable", "mongodb://" + atlasHosts + "/?tls=maybe", reportedAtlas, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := VerifiedAtlasConnection(c.uri, c.reported); got != c.want {
				t.Fatalf("VerifiedAtlasConnection = %v, want %v", got, c.want)
			}
		})
	}
}

// Write permission and identity checks for each kind of binding.
func TestBindingWriteIdentity(t *testing.T) {
	atlas := Identity{SetName: "atlas-x-shard-0", Hosts: reportedAtlas, VerifiedAtlas: true}
	unverified := atlas
	unverified.VerifiedAtlas = false
	other := atlas
	other.SetName = "atlas-y-shard-0"
	moved := atlas
	moved.Hosts = []string{"ac-b-shard-00-00.zzzz.mongodb.net:27017"}

	byID := &Binding{SetName: "rs0", ReplicaSetID: "abc", Display: "d"}
	byName := &Binding{SetName: "atlas-x-shard-0", Hosts: reportedAtlas, Display: "d"}
	byAtlas := &Binding{SetName: "atlas-x-shard-0", Hosts: reportedAtlas, AtlasTLS: true, Display: "d"}

	if err := byID.RequireReplicaSetID(); err != nil {
		t.Fatalf("ID binding may write: %v", err)
	}
	if err := byAtlas.RequireReplicaSetID(); err != nil {
		t.Fatalf("Atlas binding may write: %v", err)
	}
	if err := byName.RequireReplicaSetID(); err == nil {
		t.Fatal("a binding by name and hosts alone must not write")
	}
	if err := byName.Verify(unverified); err != nil {
		t.Fatalf("name-and-hosts binding still reads over an unverified connection: %v", err)
	}
	if err := byAtlas.Verify(atlas); err != nil {
		t.Fatalf("Atlas binding with a verified connection: %v", err)
	}
	for name, id := range map[string]Identity{"unverified connection": unverified, "other set": other, "no shared member": moved} {
		if err := byAtlas.Verify(id); err == nil {
			t.Fatalf("Atlas binding accepted %s", name)
		}
	}
	if err := atlas.RequireReplicaSetID(); err != nil {
		t.Fatalf("a verified Atlas identity may enable images at import: %v", err)
	}
	if err := unverified.RequireReplicaSetID(); err == nil {
		t.Fatal("an unverified identity without an ID must not write at import")
	}
	if !strings.Contains(byAtlas.Strength(), "TLS-verified") {
		t.Fatalf("Strength = %q", byAtlas.Strength())
	}
}
