package checkout

import (
	"errors"
	"net/url"
	"strings"

	"go.mongodb.org/mongo-driver/x/mongo/driver/connstring"
)

// sandboxCredentialURI copies deployment settings into a URI for the sandbox
// user. The input can contain engine credentials in both userinfo and options.
func sandboxCredentialURI(baseURI, database, username, password string) (string, error) {
	invalid := errors.New("invalid MongoDB deployment URI for sandbox credential")
	scheme, rest, ok := strings.Cut(baseURI, "://")
	if !ok || (scheme != "mongodb" && scheme != "mongodb+srv") || strings.ContainsAny(rest, "#\r\n\t ") {
		return "", invalid
	}
	authority, pathQuery, _ := strings.Cut(rest, "/")
	oldDB, query, _ := strings.Cut(pathQuery, "?")
	if _, err := url.PathUnescape(oldDB); err != nil || strings.ContainsAny(oldDB, "/@") || strings.Contains(authority, "?") {
		return "", invalid
	}
	// Validate MongoDB's host list and escaped userinfo with the driver. Use
	// the plain scheme for this step so rendering an SRV URI never queries DNS.
	parsed, err := connstring.Parse("mongodb://" + authority)
	if err != nil {
		return "", invalid
	}
	hosts := authority
	if _, after, ok := strings.Cut(authority, "@"); ok {
		hosts = after
	}
	if len(parsed.Hosts) != strings.Count(hosts, ",")+1 ||
		(scheme == "mongodb+srv" && strings.ContainsAny(hosts, ":,[]%")) {
		return "", invalid
	}

	out := url.Values{"authSource": {database}}
	clientCert, explicitTLS := false, false
	for pair := range strings.SplitSeq(strings.ReplaceAll(query, ";", "&"), "&") {
		if pair == "" {
			continue
		}
		rawKey, rawValue, ok := strings.Cut(pair, "=")
		key, keyErr := url.QueryUnescape(rawKey)
		value, valueErr := url.QueryUnescape(rawValue)
		if !ok || key == "" || keyErr != nil || valueErr != nil {
			return "", invalid
		}
		// Only pass known connection settings. New driver-specific options
		// may carry tokens, passwords or other client identity material.
		switch strings.ToLower(key) {
		case "tlscertificatekeyfile", "sslclientcertificatekeyfile", "tlscertificatefile", "tlsprivatekeyfile":
			clientCert = true
		case "ssl", "tls":
			explicitTLS = true
			out.Add(key, value)
		case "replicaset", "directconnection", "connect", "loadbalanced", "srvmaxhosts", "srvservicename",
			"tlscafile", "sslcertificateauthorityfile", "tlscrlfile", "tlsinsecure", "sslinsecure",
			"tlsallowinvalidcertificates", "tlsallowinvalidhostnames", "tlsdisableocspendpointcheck",
			"tlsdisablecertificaterevocationcheck", "proxyhost", "proxyport", "compressors",
			"zlibcompressionlevel", "zstdcompressionlevel", "connecttimeoutms", "heartbeatintervalms",
			"heartbeatfrequencyms", "localthresholdms", "maxidletimems", "maxpoolsize", "minpoolsize",
			"maxconnecting", "waitqueuetimeoutms", "servermonitoringmode", "serverselectiontimeoutms", "sockettimeoutms", "timeoutms",
			"retryreads", "retrywrites", "readconcernlevel", "readpreference", "readpreferencetags",
			"maxstaleness", "maxstalenessseconds", "journal", "w", "wtimeout", "wtimeoutms", "appname":
			out.Add(key, value)
		}
	}
	// A client-certificate option enables TLS even without an explicit tls
	// setting. Keep the encrypted transport when removing the engine's key.
	if clientCert && !explicitTLS {
		out.Set("tls", "true")
	}
	return scheme + "://" + url.UserPassword(username, password).String() + "@" +
		hosts + "/" + url.PathEscape(database) + "?" + out.Encode(), nil
}
