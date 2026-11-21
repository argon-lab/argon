package checkout

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/x/mongo/driver/connstring"
)

func TestSandboxCredentialURI(t *testing.T) {
	const database = "argon_br_test"
	for _, tc := range []struct {
		name, base, hosts string
		options           url.Values
	}{
		{
			name: "replica set, IPv6 and escaped engine userinfo",
			base: "mongodb://engine%40host:private%2Fsentinel@db:27018,[::1]:27019/old?" +
				"replicaSet=rs&readPreferenceTags=dc:a&readPreferenceTags=dc:b;authSource=admin&retryWrites=true",
			hosts:   "mongodb://db:27018,[::1]:27019",
			options: url.Values{"replicaSet": {"rs"}, "readPreferenceTags": {"dc:a", "dc:b"}, "retryWrites": {"true"}},
		},
		{
			name: "SRV and certificate authority",
			base: "mongodb+srv://cluster.example.invalid/?srvServiceName=servers&tlsCAFile=%2Fetc%2Fca.pem" +
				"&authMechanism=MONGODB-X509&tlsCertificateKeyFile=%2Fprivate%2Fclient.pem",
			hosts:   "mongodb+srv://cluster.example.invalid",
			options: url.Values{"srvServiceName": {"servers"}, "tlsCAFile": {"/etc/ca.pem"}, "tls": {"true"}},
		},
		{
			name:    "Unix socket",
			base:    "mongodb://root:pw@%2Ftmp%2Fmongodb-27017.sock/?directConnection=true",
			hosts:   "mongodb://%2Ftmp%2Fmongodb-27017.sock",
			options: url.Values{"directConnection": {"true"}},
		},
		{
			name: "engine authentication, certificate files and proxy credentials",
			base: "mongodb://db/old?ssl=true&sslCertificateAuthorityFile=ca.pem&sslClientCertificateKeyFile=client.pem" +
				"&TLSCertificateFile=cert.pem&tlsPrivateKeyFile=key.pem&sslClientCertificateKeyPassword=secret" +
				"&tlsCertificateKeyFilePassword=secret&AuthMechanism=MONGODB-AWS&AUTH%53ource=%24external" +
				"&authMechanismProperties=AWS_SESSION_TOKEN%3Asecret&proxyPassword=secret&proxyUsername=root" +
				"&proxyHost=proxy&proxyPort=1080&unknownToken=secret",
			hosts: "mongodb://db",
			options: url.Values{"ssl": {"true"}, "sslCertificateAuthorityFile": {"ca.pem"},
				"proxyHost": {"proxy"}, "proxyPort": {"1080"}},
		},
		{
			name:    "client certificate implies TLS",
			base:    "mongodb://db/?tlsCertificateKeyFile=engine.pem",
			hosts:   "mongodb://db",
			options: url.Values{"tls": {"true"}},
		},
		{
			name:    "without database or userinfo",
			base:    "mongodb://localhost",
			hosts:   "mongodb://localhost",
			options: url.Values{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sandboxCredentialURI(tc.base, database, "sandbox@user", "new/p:w")
			require.NoError(t, err)
			scheme, hosts, _ := strings.Cut(tc.hosts, "://")
			tc.options.Set("authSource", database)
			want := scheme + "://sandbox%40user:new%2Fp%3Aw@" + hosts + "/" + database + "?" + tc.options.Encode()
			require.Equal(t, want, got)
			if scheme == "mongodb" {
				parsed, err := connstring.ParseAndValidate(got)
				require.NoError(t, err)
				require.Equal(t, "new/p:w", parsed.Password)
				require.Equal(t, database, parsed.AuthSource)
			}
		})
	}
}

func TestSandboxCredentialURIInvalid(t *testing.T) {
	for _, base := range []string{
		"", "https://engine:private@db", "mongodb://engine:private@/db",
		"mongodb://engine:%xxprivate@db", "mongodb://engine:private@db:invalid",
		"mongodb://engine:private@first,,second", "mongodb://engine:private@db/old/extra",
		"mongodb://engine:private@db/?replicaSet=private%xx", "mongodb://engine:private@db/?replicaSet",
		"mongodb://engine:private@db/?=private",
		"mongodb://engine:private@db/db#private", "mongodb+srv://engine:private@first,second",
		"mongodb+srv://engine:private@cluster.example.org:27017",
	} {
		got, err := sandboxCredentialURI(base, "argon_br_test", "sandbox", "new-private")
		require.EqualError(t, err, "invalid MongoDB deployment URI for sandbox credential")
		require.Empty(t, got)
	}
}
