package checkout

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/argon-lab/argon/v2/internal/wal"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

// ErrCredentialUnavailable reports that this deployment cannot issue sandbox
// credentials: it does not enforce authentication, or the engine may not
// manage users. It describes the deployment, not the request.
var ErrCredentialUnavailable = errors.New("sandbox credentials are unavailable on this deployment")

// atlasCommandNotAllowed is the code MongoDB Atlas returns (AtlasError,
// CMD_NOT_ALLOWED) for commands it reserves to its own control plane.
const atlasCommandNotAllowed = 8000

// RequireSandboxAuthentication checks the engine's own authenticated session.
func (s *Service) RequireSandboxAuthentication(ctx context.Context) error {
	var status struct {
		AuthInfo struct {
			AuthenticatedUsers []bson.Raw `bson:"authenticatedUsers"`
		} `bson:"authInfo"`
	}
	if err := s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "connectionStatus", Value: 1}}).Decode(&status); err != nil {
		return fmt.Errorf("cannot check MongoDB authentication for sandbox credentials: %w", err)
	}
	if len(status.AuthInfo.AuthenticatedUsers) == 0 {
		return fmt.Errorf("%w: the deployment does not enforce authentication for the engine connection (connectionStatus reports no authenticated user); sandbox credentials require an authenticated deployment", ErrCredentialUnavailable)
	}
	return nil
}

// IssueSandboxCredential returns the only copy of the credential URI. On
// failure mayExist distinguishes a definite rejection from a lost reply.
func (s *Service) IssueSandboxCredential(ctx context.Context, branch *wal.Branch, baseURI string) (uri string, mayExist bool, err error) {
	db := s.client.Database(PhysicalDBName(branch.ID))
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", false, fmt.Errorf("generate sandbox password: %w", err)
	}
	password := base64.RawURLEncoding.EncodeToString(secret[:])
	uri, err = sandboxCredentialURI(baseURI, db.Name(), branch.SandboxUsername, password)
	if err != nil {
		return "", false, err
	}
	issued := false
	err = s.branches.IssueSandboxUser(ctx, branch, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if issued {
			return nil // The metadata transaction retried after createUser succeeded.
		}
		// Wait for the server's answer even if the requester disconnects.
		// User-management commands cannot participate in the metadata transaction.
		commandCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()
		err := db.RunCommand(commandCtx, bson.D{
			{Key: "createUser", Value: branch.SandboxUsername},
			{Key: "pwd", Value: password},
			{Key: "roles", Value: bson.A{bson.M{"role": "readWrite", "db": db.Name()}}},
			{Key: "writeConcern", Value: bson.M{"w": "majority"}},
			{Key: "maxTimeMS", Value: 15000},
		}).Err()
		var denied mongo.CommandError
		if errors.As(err, &denied) && denied.Code == 13 {
			return fmt.Errorf("%w: MongoDB denied createUser: the engine needs userAdmin on the sandbox databases or userAdminAnyDatabase (on MongoDB Atlas, which grants neither, create the user in the Atlas UI or with the Atlas Admin API)", ErrCredentialUnavailable)
		}
		if errors.As(err, &denied) && denied.Code == atlasCommandNotAllowed {
			// MongoDB Atlas manages database users through its own API and UI
			// and refuses the createUser command on every tier.
			return fmt.Errorf("%w: MongoDB Atlas does not allow createUser; create a user scoped to the sandbox database in the Atlas UI or with the Atlas Admin API, or run the engine on a deployment you administer", ErrCredentialUnavailable)
		}
		mayExist = true
		if err != nil {
			// Command errors may echo the request, including pwd.
			if errors.As(err, &denied) {
				return fmt.Errorf("createUser failed on %s (MongoDB code %d)", db.Name(), denied.Code)
			}
			return fmt.Errorf("could not confirm createUser on %s", db.Name())
		}
		issued = true
		return ctx.Err()
	})
	if err != nil {
		return "", mayExist, err
	}
	return uri, true, nil
}

func (s *Service) revokeSandboxUser(ctx context.Context, branch *wal.Branch) error {
	// Derive the authentication database from the branch ID. Never target a
	// database or a user supplied by a mismatched checkout record.
	db := PhysicalDBName(branch.ID)
	if branch.SandboxUsername != "sandbox_"+branch.ID || (branch.PhysicalDB != "" && branch.PhysicalDB != db) || branch.IsFollower() {
		return fmt.Errorf("refusing to revoke a mismatched sandbox credential on branch %q", branch.Name)
	}
	err := s.client.Database(db).RunCommand(ctx, bson.D{
		{Key: "dropUser", Value: branch.SandboxUsername},
		{Key: "writeConcern", Value: bson.M{"w": "majority"}},
	}).Err()
	var missing mongo.CommandError
	if errors.As(err, &missing) && missing.Code == 11 { // UserNotFound on a cleanup retry.
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to revoke sandbox credential on %s: %w", db, err)
	}
	return nil
}
