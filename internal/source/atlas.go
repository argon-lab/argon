package source

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"sync"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/x/mongo/driver/connstring"
	"golang.org/x/crypto/x509roots/fallback/bundle"
)

// atlasDomain is the DNS domain of MongoDB Atlas cluster hosts. Atlas serves
// them only over TLS, with certificates from public certificate authorities.
const atlasDomain = ".mongodb.net"

// IdentifyAt is Identify for a client made by Connect from uri. It also
// reports whether the connection reaches MongoDB Atlas with full TLS
// verification against the public certificate authorities.
//
// Atlas's shared tiers (M0, Flex) refuse replSetGetConfig, so a source there
// has no replica-set ID. The set name and member host strings alone could be
// presented by a tunnel or another deployment. On Atlas they cannot, provided
// the trust is public: every member is a host under mongodb.net, and only
// Atlas can obtain a certificate for those names from a public authority.
// The machine's own trust store is not enough for this, since it may also
// trust a private root (a corporate root, mkcert, SSL_CERT_FILE), so every
// handshake of the client is checked again against the Mozilla root program
// shipped with Argon (see Connect). That verified connection is the identity,
// and it is accepted for writes in place of the ID.
func IdentifyAt(ctx context.Context, client *mongo.Client, uri string) (Identity, error) {
	id, err := Identify(ctx, client)
	if err != nil {
		return id, err
	}
	rec, _ := handshakes.Load(client)
	recorder, _ := rec.(*handshakeRecorder)
	id.VerifiedAtlas = VerifiedAtlasConnection(uri, id.Hosts) && recorder != nil && recorder.allPublic()
	if id.VerifiedAtlas {
		// From now on a handshake that public roots cannot verify fails, so
		// a later connection of this client cannot reach another server.
		recorder.enforce()
	}
	return id, nil
}

// VerifiedAtlasConnection reports whether uri connects to Atlas hosts with the
// driver's full certificate, host name and revocation checks, and every
// member the deployment reported is an Atlas host. Any option that weakens
// verification, a custom certificate authority or a host outside mongodb.net
// makes it false. It does not establish which roots verified the servers;
// IdentifyAt adds that.
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
	if opts.DisableOCSPEndpointCheck != nil && *opts.DisableOCSPEndpointCheck {
		return false
	}
	return allAtlasHosts(opts.Hosts) && allAtlasHosts(reported)
}

// weakensTLS finds options that turn off part of certificate, host name or
// revocation checking, whether or not this driver honours them. It reads the
// options as the driver's own parser does, so characters elsewhere in the
// string, such as a '?' in the password, cannot hide one.
func weakensTLS(uri string) bool {
	cs, err := connstring.Parse(uri)
	if err != nil {
		return true
	}
	for _, opts := range []map[string][]string{cs.Options, cs.UnknownOptions} {
		for key, vals := range opts {
			switch strings.ToLower(key) {
			case "tlscafile", "sslcertificateauthorityfile":
				return true
			case "tlsinsecure", "sslinsecure", "tlsallowinvalidcertificates", "tlsallowinvalidhostnames",
				"sslallowinvalidcertificates", "sslallowinvalidhostnames", "tlsdisableocspendpointcheck",
				"tlsdisablecertificaterevocationcheck":
				for _, v := range vals {
					if !strings.EqualFold(v, "false") {
						return true
					}
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

// handshakes holds the recorder of every client Connect made.
var handshakes sync.Map // *mongo.Client -> *handshakeRecorder

// handshakeRecorder checks each TLS handshake of one client against the
// public roots, after the driver's own verification has passed. Until
// enforced it only records; that keeps sources behind a TLS-inspecting proxy
// readable, while they cannot identify as Atlas.
type handshakeRecorder struct {
	mu        sync.Mutex
	seen      int
	untrusted int
	enforced  bool
}

func (r *handshakeRecorder) verify(state tls.ConnectionState) error {
	public := verifiesPublicly(state)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen++
	if !public {
		r.untrusted++
		if r.enforced {
			return errors.New("the server's certificate does not chain to a public certificate authority, and this connection was identified as MongoDB Atlas")
		}
	}
	return nil
}

func (r *handshakeRecorder) allPublic() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seen > 0 && r.untrusted == 0
}

func (r *handshakeRecorder) enforce() {
	r.mu.Lock()
	r.enforced = true
	r.mu.Unlock()
}

// verifiesPublicly reports whether the server's certificate chain, as sent in
// the handshake, verifies for the name the client asked for against the
// public roots alone.
func verifiesPublicly(state tls.ConnectionState) bool {
	if len(state.PeerCertificates) == 0 || state.ServerName == "" {
		return false
	}
	intermediates := x509.NewCertPool()
	for _, cert := range state.PeerCertificates[1:] {
		intermediates.AddCert(cert)
	}
	_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
		DNSName:       state.ServerName,
		Roots:         publicRoots(),
		Intermediates: intermediates,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	return err == nil
}

var publicRoots = sync.OnceValue(func() *x509.CertPool {
	pool := x509.NewCertPool()
	for root := range bundle.Roots() {
		cert, err := x509.ParseCertificate(root.Certificate)
		if err != nil {
			continue
		}
		if root.Constraint != nil {
			pool.AddCertWithConstraint(cert, root.Constraint)
		} else {
			pool.AddCert(cert)
		}
	}
	return pool
})

// recordHandshakes makes every TLS handshake of a client built from opts go
// through a new recorder, which Connect files under the client.
func recordHandshakes(opts *options.ClientOptions) *handshakeRecorder {
	if opts.TLSConfig == nil {
		return nil
	}
	rec := &handshakeRecorder{}
	cfg := opts.TLSConfig.Clone()
	cfg.VerifyConnection = rec.verify
	opts.SetTLSConfig(cfg)
	return rec
}
