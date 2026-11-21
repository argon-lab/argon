package source

import (
	"context"
	"net"
	"net/url"
	"strings"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// atlasDomain is the DNS domain of MongoDB Atlas cluster hosts. Atlas serves
// them only over TLS with certificates from a public certificate authority.
const atlasDomain = ".mongodb.net"

// IdentifyAt is Identify for a connection made from uri. It also reports
// whether the connection reaches MongoDB Atlas with full TLS verification.
//
// Atlas's shared tiers (M0, Flex) refuse replSetGetConfig, so a source there
// has no replica-set ID. The set name and member host strings alone could be
// presented by a tunnel or another deployment. On Atlas they cannot: every
// member is a host under mongodb.net, the client verifies each server's
// certificate for that host name against the system's public roots, and
// only Atlas can obtain such certificates. That verified connection is the
// identity, and it is accepted for writes in place of the ID.
func IdentifyAt(ctx context.Context, client *mongo.Client, uri string) (Identity, error) {
	id, err := Identify(ctx, client)
	if err != nil {
		return id, err
	}
	id.VerifiedAtlas = VerifiedAtlasConnection(uri, id.Hosts)
	return id, nil
}

// VerifiedAtlasConnection reports whether uri connects to Atlas hosts with the
// driver's full certificate and host name verification against the system
// roots, and every member the deployment reported is an Atlas host. Any
// option that weakens verification, a custom certificate authority or a
// host outside mongodb.net makes it false.
func VerifiedAtlasConnection(uri string, reported []string) bool {
	if weakensTLS(uri) {
		return false
	}
	opts := options.Client().ApplyURI(uri)
	if opts.Validate() != nil {
		return false
	}
	tlsConfig := opts.TLSConfig
	if tlsConfig == nil || tlsConfig.InsecureSkipVerify || tlsConfig.RootCAs != nil ||
		tlsConfig.VerifyPeerCertificate != nil || tlsConfig.VerifyConnection != nil {
		return false
	}
	return allAtlasHosts(opts.Hosts) && allAtlasHosts(reported)
}

// weakensTLS finds options in the connection string that turn off part of
// certificate or host name checking, whether or not this driver honours them.
func weakensTLS(uri string) bool {
	_, rest, ok := strings.Cut(uri, "?")
	if !ok {
		return false
	}
	values, err := url.ParseQuery(strings.ReplaceAll(rest, ";", "&"))
	if err != nil {
		return true
	}
	for key, vals := range values {
		switch strings.ToLower(key) {
		case "tlsinsecure", "sslinsecure", "tlsallowinvalidcertificates", "tlsallowinvalidhostnames",
			"sslallowinvalidcertificates", "sslallowinvalidhostnames", "tlscafile", "sslcertificateauthorityfile":
			for _, v := range vals {
				if !strings.EqualFold(v, "false") || strings.HasSuffix(strings.ToLower(key), "file") {
					return true
				}
			}
		}
	}
	return false
}

func allAtlasHosts(hosts []string) bool {
	if len(hosts) == 0 {
		return false
	}
	for _, h := range hosts {
		if !isAtlasHost(h) {
			return false
		}
	}
	return true
}

func isAtlasHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !strings.HasSuffix(host, atlasDomain) || len(host) == len(atlasDomain) {
		return false
	}
	// Only a plain DNS name qualifies: letters, digits, hyphens and dots.
	for _, r := range host {
		plain := r == '.' || r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !plain {
			return false
		}
	}
	return !strings.Contains(host, "..")
}
