package dokploy

import (
	"context"
	"errors"
	"testing"
)

func TestNewClientFromEnvRequiresURLAndToken(t *testing.T) {
	t.Setenv(EnvBaseURL, "")
	t.Setenv(EnvToken, "")
	if _, err := NewClientFromEnv(); err == nil {
		t.Fatalf("expected error when URL is missing")
	}
	t.Setenv(EnvBaseURL, "https://dokploy.example")
	if _, err := NewClientFromEnv(); err == nil {
		t.Fatalf("expected error when token is missing")
	}
	t.Setenv(EnvToken, "secret")
	client, err := NewClientFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.BaseURL != "https://dokploy.example" || client.Token != "secret" {
		t.Fatalf("unexpected client: %+v", client)
	}
}

func TestApplyReturnsNotImplemented(t *testing.T) {
	client := &Client{BaseURL: "https://dokploy.example", Token: "secret"}
	plan := Plan{Steps: []Step{{Kind: StepCreateProject, App: "api", Ref: "api"}}}
	err := client.Apply(context.Background(), plan)
	if !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("expected ErrNotImplemented, got %v", err)
	}
}
