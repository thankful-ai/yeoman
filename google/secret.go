package google

import (
	"context"
	"encoding/json"
	"fmt"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	"github.com/egtann/yeoman"
	secretmanagerpb "google.golang.org/genproto/googleapis/cloud/secretmanager/v1"
)

var _ yeoman.Store = &Secret{}

type Secret struct {
	ProjectID string
}

const storeServicesName = "ym-services"

func (s *Secret) GetServices(
	ctx context.Context,
) ([]yeoman.ServiceOpt, error) {
	byt, err := s.get(ctx, storeServicesName)
	if err != nil {
		return nil, fmt.Errorf("get: %w", err)
	}
	var opts []yeoman.ServiceOpt
	if err = json.Unmarshal(byt, &opts); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return opts, nil
}

func (s *Secret) SetServices(
	ctx context.Context,
	opts []yeoman.ServiceOpts,
) error {
	byt, err := json.Marshal(opts)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err = s.set(ctx, storeServicesName, byt); err != nil {
		return fmt.Errorf("set: %w", err)
	}
	return nil
}

func (s *Secret) get(ctx context.Context, name string) ([]byte, error) {
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	defer client.Close()

	accessRequest := &secretmanagerpb.AccessSecretVersionRequest{
		Name: fmt.Sprintf("projects/%s/secrets/%s/versions/latest",
			s.ProjectID, name),
	}
	result, err := client.AccessSecretVersion(ctx, accessRequest)
	if err != nil {
		return nil, fmt.Errorf("access secret version: %w", err)
	}
	return result.Payload.Data, nil
}

func (s *Secret) set(ctx context.Context, name string, data []byte) error {
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("new client: %w", err)
	}
	defer client.Close()

	createSecretReq := &secretmanagerpb.CreateSecretRequest{
		Parent:   fmt.Sprintf("projects/%s", s.ProjectID),
		SecretId: name,
		Secret: &secretmanagerpb.Secret{
			Replication: &secretmanagerpb.Replication{
				Replication: &secretmanagerpb.Replication_Automatic_{
					Automatic: &secretmanagerpb.Replication_Automatic{},
				},
			},
		},
	}
	secret, err := client.CreateSecret(ctx, createSecretReq)
	if err != nil {
		return fmt.Errorf("create secret: %w", err)
	}

	addSecretVersionReq := &secretmanagerpb.AddSecretVersionRequest{
		Parent: secret.Name,
		Payload: &secretmanagerpb.SecretPayload{
			Data: data,
		},
	}
	_, err = client.AddSecretVersion(ctx, addSecretVersionReq)
	if err != nil {
		return fmt.Errorf("add secret version: %w", err)
	}
	return nil
}
