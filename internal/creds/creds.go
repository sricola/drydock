// Package creds issues credentials for a task. MVP: a static key read from
// the broker host. The Provider interface lets a gateway-backed, per-task
// token provider replace it later without changing callers.
package creds

import (
	"errors"
	"time"
)

type Token struct {
	Value string
}

type Provider interface {
	Mint(ttl time.Duration) (Token, error)
	Revoke(Token) error
}

type StaticProvider struct {
	Key string
}

func (p StaticProvider) Mint(time.Duration) (Token, error) {
	if p.Key == "" {
		return Token{}, errors.New("creds: empty static key")
	}
	return Token{Value: p.Key}, nil
}

func (StaticProvider) Revoke(Token) error { return nil }
