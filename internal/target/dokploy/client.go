package dokploy

import (
	"errors"
	"fmt"
	"os"
)

const (
	EnvBaseURL = "BORT_DOKPLOY_URL"
	EnvToken   = "BORT_DOKPLOY_TOKEN"
)

var ErrNotImplemented = errors.New("dokploy live mode is not implemented")

type Client struct {
	BaseURL string
	Token   string
}

func NewClientFromEnv() (*Client, error) {
	baseURL := os.Getenv(EnvBaseURL)
	token := os.Getenv(EnvToken)
	if baseURL == "" {
		return nil, fmt.Errorf("%s is required for live mode", EnvBaseURL)
	}
	if token == "" {
		return nil, fmt.Errorf("%s is required for live mode", EnvToken)
	}
	return &Client{BaseURL: baseURL, Token: token}, nil
}
