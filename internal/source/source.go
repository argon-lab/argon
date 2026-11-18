// Package source records which external MongoDB database a branch follows
// and keeps Argon's destructive paths away from it.
//
// An online import binds the new project's main branch to the database it
// was copied from. The binding holds no connection string: the URI is given
// again on every command that reaches the source, and what is stored is only
// enough to recognise that deployment and to show where it is without
// credentials.
package source

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// ManagedPrefix is the prefix of every database Argon creates and may drop.
// A source database can never carry it, so no drop path can name a source.
const ManagedPrefix = "argon_br_"

const collectionName = "wal_sources"

// Binding ties one branch to the external database it follows.
type Binding struct {
	BranchID  string `bson:"_id" json:"branch_id"`
	ProjectID string `bson:"project_id" json:"project_id"`
	Database  string `bson:"database" json:"database"`
	// Display is the source location without credentials or options.
	Display string `bson:"display" json:"display"`
	// SetName and ReplicaSetID are recorded at import. A later connection must
	// present the same ID if the binding has one.
	SetName      string `bson:"set_name" json:"set_name"`
	ReplicaSetID string `bson:"replica_set_id,omitempty" json:"replica_set_id,omitempty"`
	// Hosts are the replica-set members the deployment reported. When the
	// replica-set ID could not be read they are the second half of the
	// identity: a set name alone ("rs0") says almost nothing.
	Hosts     []string  `bson:"hosts,omitempty" json:"hosts,omitempty"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
}

// Identity is what a deployment reports about itself.
type Identity struct {
	SetName      string
	ReplicaSetID string
	Hosts        []string
	Sharded      bool
}

// Registry stores bindings in the Argon metadata database.
type Registry struct {
	bindings   *mongo.Collection
	metadataDB string
}

// NewRegistry opens the bindings collection in the metadata database.
func NewRegistry(metaDB *mongo.Database) (*Registry, error) {
	r := &Registry{bindings: metaDB.Collection(collectionName), metadataDB: metaDB.Name()}
	_, err := r.bindings.Indexes().CreateMany(context.Background(), []mongo.IndexModel{
		{Keys: bson.D{{Key: "project_id", Value: 1}}},
		{Keys: bson.D{{Key: "database", Value: 1}}},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create source indexes: %w", err)
	}
	return r, nil
}

// ValidateDatabase refuses source names that Argon itself owns. Because a
// bound source can never be named like a physical branch database or like
// the metadata database, the paths that drop those can never reach it.
func ValidateDatabase(name, metadataDB string) error {
	switch {
	case name == "":
		return errors.New("source database name is required")
	case strings.HasPrefix(name, ManagedPrefix):
		return fmt.Errorf("database %q is an Argon branch database (prefix %s): Argon creates and drops those itself, so it cannot be followed as a source", name, ManagedPrefix)
	case name == metadataDB:
		return fmt.Errorf("database %q is Argon's metadata database and cannot be followed as a source", name)
	case name == "admin" || name == "local" || name == "config":
		return fmt.Errorf("database %q is a MongoDB system database and cannot be followed as a source", name)
	}
	return nil
}

// Bind records a binding. It runs inside the caller's session when ctx is one.
func (r *Registry) Bind(ctx context.Context, binding *Binding) error {
	if err := ValidateDatabase(binding.Database, r.metadataDB); err != nil {
		return err
	}
	if binding.CreatedAt.IsZero() {
		binding.CreatedAt = time.Now()
	}
	if _, err := r.bindings.InsertOne(ctx, binding); err != nil {
		return fmt.Errorf("failed to record the source binding: %w", err)
	}
	return nil
}

// Get returns a branch's binding, or nil when the branch follows nothing.
func (r *Registry) Get(ctx context.Context, branchID string) (*Binding, error) {
	var binding Binding
	err := r.bindings.FindOne(ctx, bson.M{"_id": branchID}).Decode(&binding)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &binding, nil
}

// Unbind removes a branch's binding.
func (r *Registry) Unbind(ctx context.Context, branchID string) error {
	_, err := r.bindings.DeleteOne(ctx, bson.M{"_id": branchID})
	return err
}

// GuardDrop refuses to drop a database that any branch follows. Checkout
// calls it before each drop, in addition to checking that the name is the
// branch's own managed database.
func (r *Registry) GuardDrop(ctx context.Context, database string) error {
	if !strings.HasPrefix(database, ManagedPrefix) {
		return fmt.Errorf("refusing to drop %q: Argon drops only its own %s* databases", database, ManagedPrefix)
	}
	count, err := r.bindings.CountDocuments(ctx, bson.M{"database": database})
	if err != nil {
		return fmt.Errorf("refusing to drop %q: could not check it against followed sources: %w", database, err)
	}
	if count > 0 {
		return fmt.Errorf("refusing to drop %q: it is a followed source database", database)
	}
	return nil
}

// Verify checks the identity recorded at import. When the binding lacks an ID,
// the set name and at least one member host must match. That check is suitable
// only for read operations because different deployments can share the strings.
func (b *Binding) Verify(id Identity) error {
	if b.ReplicaSetID != "" {
		if id.ReplicaSetID == "" {
			return fmt.Errorf("this project recorded its source's replica-set ID, and this connection is not allowed to read it (replSetGetConfig), so it cannot be shown to lead to the same deployment (%s). Connect as a user that may run replSetGetConfig, for example one with the clusterMonitor role", b.Display)
		}
		if b.ReplicaSetID != id.ReplicaSetID {
			return fmt.Errorf("the connection leads to a different deployment than the one this project follows (%s)", b.Display)
		}
		return nil
	}
	if b.SetName != id.SetName {
		return fmt.Errorf("the connection leads to replica set %q, but this project follows %q (%s)", id.SetName, b.SetName, b.Display)
	}
	if !sharesHost(b.Hosts, id.Hosts) {
		return fmt.Errorf("the connection leads to a replica set named %q with none of the members this project's source had when it was imported (%s). If this is the intended source and its members have changed, %s", id.SetName, b.Display, reimportWithID)
	}
	return nil
}

const replicaSetIDRequired = "a replica-set ID is required because a tunnel or another deployment can present the same replica-set name and member host strings"
const configReader = "a user allowed to run replSetGetConfig, for example one with the clusterMonitor role"
const reimportWithID = "stop any running follower, run `argon source detach -p <project> --yes`, then import online into a new project with " + configReader

// RequireReplicaSetID refuses source writes unless the binding recorded an ID.
func (b *Binding) RequireReplicaSetID() error {
	if b.ReplicaSetID == "" {
		return errors.New(replicaSetIDRequired + ". To bind the source again, " + reimportWithID)
	}
	return nil
}

// RequireReplicaSetID refuses source writes during import unless the ID can be recorded.
func (id Identity) RequireReplicaSetID() error {
	if id.ReplicaSetID == "" {
		return errors.New(replicaSetIDRequired + ". To record the ID, import online with " + configReader)
	}
	return nil
}

// Strength says what a binding's identity check rests on.
func (b *Binding) Strength() string {
	if b.ReplicaSetID != "" {
		return "replica-set ID"
	}
	return "replica-set name and member hosts"
}

func sharesHost(recorded, reported []string) bool {
	for _, a := range recorded {
		for _, b := range reported {
			if strings.EqualFold(a, b) {
				return true
			}
		}
	}
	return false
}

// Identify asks a deployment who it is. A source has to be a replica set:
// both the consistent import and following it read its change stream.
func Identify(ctx context.Context, client *mongo.Client) (Identity, error) {
	var hello struct {
		SetName  string   `bson:"setName"`
		Msg      string   `bson:"msg"`
		Hosts    []string `bson:"hosts"`
		Passives []string `bson:"passives"`
	}
	admin := client.Database("admin")
	primary := options.RunCmd().SetReadPreference(readpref.Primary())
	if err := admin.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}, primary).Decode(&hello); err != nil {
		return Identity{}, err
	}
	id := Identity{SetName: hello.SetName, Sharded: hello.Msg == "isdbgrid", Hosts: append(hello.Hosts, hello.Passives...)}
	if id.Sharded {
		return id, errors.New("the source is a sharded cluster (mongos); online import, follow and write-back are only supported, and only tested, against a replica set")
	}
	if id.SetName == "" {
		return id, errors.New("the source is a standalone mongod; online import and follow read the change stream, which needs a replica set. Use a quiesced import (--source-quiesced) for a standalone")
	}
	// The replica-set ID distinguishes separately initialized deployments.
	// Reading it needs a privilege some source users lack. A server that
	// refuses the command leaves the ID empty. A network failure is an error.
	var config struct {
		Config struct {
			Settings struct {
				ReplicaSetID primitive.ObjectID `bson:"replicaSetId"`
			} `bson:"settings"`
		} `bson:"config"`
	}
	err := admin.RunCommand(ctx, bson.D{{Key: "replSetGetConfig", Value: 1}}, primary).Decode(&config)
	var refused mongo.CommandError
	switch {
	case err == nil:
		if !config.Config.Settings.ReplicaSetID.IsZero() {
			id.ReplicaSetID = config.Config.Settings.ReplicaSetID.Hex()
		}
	case errors.As(err, &refused) && !mongo.IsNetworkError(err):
		// The server refused the command. Driver network failures can also
		// arrive as CommandError, with the NetworkError label.
	default:
		return id, fmt.Errorf("failed to read the source's replica-set configuration: %w", err)
	}
	return id, nil
}

// Display renders where a connection string points without its credentials
// or options (options can carry tokens). It only reads the string: a
// mongodb+srv name is shown as written, not resolved.
func Display(uri, database string) string {
	scheme, rest, ok := strings.Cut(uri, "://")
	if !ok || (scheme != "mongodb" && scheme != "mongodb+srv") {
		return "(location withheld: not a MongoDB connection string)/" + database
	}
	if end := strings.IndexAny(rest, "/?"); end >= 0 {
		rest = rest[:end]
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	if rest == "" {
		return "(location withheld: the connection string names no host)/" + database
	}
	return scheme + "://" + rest + "/" + database
}

// RedactError returns err with everything secret in the connection string
// removed from its message. Driver errors are wrapped before they reach a
// log line, a status document or the terminal.
func RedactError(err error, uri string) error {
	if err == nil {
		return nil
	}
	message := Redact(err.Error(), uri)
	if message == err.Error() {
		return err
	}
	return &redactedError{message: message, cause: err}
}

// redactedError keeps the cause for errors.Is / errors.As while printing
// only the cleaned message.
type redactedError struct {
	message string
	cause   error
}

func (e *redactedError) Error() string { return e.message }
func (e *redactedError) Unwrap() error { return e.cause }

// minStandaloneSecret is the shortest password removed as a bare substring.
const minStandaloneSecret = 4

// secretOptions are the connection-string options whose values are secrets.
// Other option values (replica set name, read preference, file paths) are
// left alone: masking them only makes errors unreadable.
var secretOptions = map[string]bool{
	"tlscertificatekeyfilepassword":   true,
	"sslclientcertificatekeypassword": true,
	"authmechanismproperties":         true,
	"proxypassword":                   true,
}

// Redact removes a connection string's secrets from arbitrary text: the
// string itself, the user:password pair, the password in every escaping, and
// the values of options that carry secrets. A user name on its own is not
// removed.
func Redact(text, uri string) string {
	if uri == "" {
		return text
	}
	const mask = "[redacted]"
	seen := map[string]bool{}
	var secrets []string
	add := func(values ...string) {
		for _, value := range values {
			if value != "" && !seen[value] {
				seen[value] = true
				secrets = append(secrets, value)
			}
		}
	}
	add(uri)
	if scheme := strings.Index(uri, "://"); scheme >= 0 {
		rest := uri[scheme+3:]
		// User information ends at the last '@' of the authority. A string
		// with an unescaped '/' or '?' in its password is not a valid URI,
		// but it is still somebody's password: take the last '@' of the
		// whole string too.
		ends := []int{strings.LastIndex(rest, "@")}
		if stop := strings.IndexAny(rest, "/?"); stop >= 0 {
			ends = append(ends, strings.LastIndex(rest[:stop], "@"))
		}
		for _, end := range ends {
			if end <= 0 {
				continue
			}
			userinfo := rest[:end]
			add(userinfo)
			// On its own the password is removed wherever it occurs, unless
			// it is so short that doing so would cut up ordinary words (a
			// password of "a" would take every "a" out of the message). Such
			// a password is still removed with the string and the pair.
			if _, password, ok := strings.Cut(userinfo, ":"); ok && len(password) >= minStandaloneSecret {
				add(password)
				if decoded, err := url.PathUnescape(password); err == nil && len(decoded) >= minStandaloneSecret {
					add(decoded, url.QueryEscape(decoded), url.PathEscape(decoded))
				}
			}
		}
		if q := strings.Index(rest, "?"); q >= 0 {
			for _, pair := range strings.Split(rest[q+1:], "&") {
				key, value, ok := strings.Cut(pair, "=")
				if !ok || !secretOptions[strings.ToLower(key)] {
					continue
				}
				add(value)
				if decoded, err := url.QueryUnescape(value); err == nil {
					add(decoded)
					// authMechanismProperties is NAME:value,NAME:value.
					for _, property := range strings.Split(decoded, ",") {
						if _, secret, ok := strings.Cut(property, ":"); ok {
							add(secret)
						}
					}
				}
			}
		}
	}
	// One pass, longest first: a later replacement never rescans a mask an
	// earlier one inserted, and a secret that contains another is removed
	// whole.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	pairs := make([]string, 0, 2*len(secrets))
	for _, secret := range secrets {
		pairs = append(pairs, secret, mask)
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// Connect opens a client on a source deployment and checks that it answers.
// Every error it returns has been through RedactError.
func Connect(ctx context.Context, uri string) (*mongo.Client, error) {
	if uri == "" {
		return nil, errors.New("a source connection string is required (--uri or ARGON_SOURCE_URI); Argon does not store it")
	}
	clientOptions := options.Client().ApplyURI(uri).SetAppName("argon-source")
	if err := clientOptions.Validate(); err != nil {
		// The parser's message can quote the string it was given.
		return nil, fmt.Errorf("the source connection string cannot be used: %w", RedactError(err, uri))
	}
	client, err := mongo.Connect(ctx, clientOptions)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to the source: %w", RedactError(err, uri))
	}
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		disconnect, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Disconnect(disconnect)
		return nil, fmt.Errorf("the source did not answer: %w", RedactError(err, uri))
	}
	return client, nil
}

// SkipCollection reports collections that are neither copied nor followed by
// an online import. Only the system.* namespace is reserved by MongoDB
// inside a user database; everything else is the application's data. The
// import and the follower must agree on this exactly: a collection one of
// them skips and the other does not would end up half present.
func SkipCollection(name string) bool {
	return strings.HasPrefix(name, "system.")
}

// EnableImages turns on changeStreamPreAndPostImages for existing source
// collections and returns the ones it changed, also when it stops at an
// error. It changes how the source stores writes (MongoDB keeps a pre-image
// of every updated or deleted document), so it runs only when the operator
// asked for it by name. It uses collMod alone: a collection that disappeared
// in the meantime is reported, never created. Argon does not turn the
// setting off again.
func EnableImages(ctx context.Context, db *mongo.Database, collections []string) ([]string, error) {
	var changed []string
	for _, name := range collections {
		if err := db.RunCommand(ctx, bson.D{
			{Key: "collMod", Value: name},
			{Key: "changeStreamPreAndPostImages", Value: bson.M{"enabled": true}},
		}).Err(); err != nil {
			// A collMod that was interrupted may still have taken effect.
			return changed, ImagesLeftEnabled(fmt.Errorf("enabling change-stream pre- and post-images on source collection %s did not report success (%w); check that collection, the change may have been made", name, err), changed)
		}
		changed = append(changed, name)
	}
	return changed, nil
}

// ImagesLeftEnabled adds to an error the fact that the source was altered
// and stays altered. A failure after EnableImages must say so: nothing else
// tells the operator that production now records pre-images.
func ImagesLeftEnabled(err error, collections []string) error {
	if err == nil || len(collections) == 0 {
		return err
	}
	return fmt.Errorf("%w. changeStreamPreAndPostImages was enabled on source collection(s) %s and is still enabled; Argon does not turn it off (collMod with changeStreamPreAndPostImages: {enabled: false} does)", err, strings.Join(collections, ", "))
}

// TimeSeries lists the time-series collections of a source database. Their
// measurements live in system.buckets.* and never reach a change stream as
// documents, so a source that has one cannot be followed.
func TimeSeries(ctx context.Context, db *mongo.Database) ([]string, error) {
	specs, err := db.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, spec := range specs {
		if spec.Type == "timeseries" {
			names = append(names, spec.Name)
		}
	}
	return names, nil
}

// Collections lists the ordinary collections of a source database that an
// import copies and a follower follows.
func Collections(ctx context.Context, db *mongo.Database) ([]string, error) {
	specs, err := db.ListCollectionSpecifications(ctx, bson.D{})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, spec := range specs {
		if spec.Type == "collection" && !SkipCollection(spec.Name) {
			names = append(names, spec.Name)
		}
	}
	return names, nil
}
