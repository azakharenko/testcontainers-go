package testcontainers

import (
	"context"

	"github.com/pkg/errors"
)

// GenericContainerRequest represents parameters to a generic container
type GenericContainerRequest struct {
	ContainerRequest              // embedded request for provider
	Started          bool         // whether to auto-start the container
	ProviderType     ProviderType // which provider to use, Docker if empty
}

// GenericContainer creates a generic container with parameters
func GenericContainer(ctx context.Context, req GenericContainerRequest) (Container, error) {
	provider, err := req.ProviderType.GetProvider()
	if err != nil {
		return nil, err
	}

	c, err := provider.CreateContainer(ctx, req.ContainerRequest)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create container")
	}

	if req.Started {
		if err := c.Start(ctx); err != nil {
			return c, errors.Wrap(err, "failed to start container")
		}
	}

	return c, nil
}

// UseExistent uses existent container
func UseExistent(ctx context.Context, req GenericContainerRequest) (Container, error) {
	provider, err := req.ProviderType.GetProvider()
	if err != nil {
		return nil, err
	}

	c, err := provider.CreateFromExistentContainer(ctx, req.Name)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create container")
	}

	return c, nil
}
