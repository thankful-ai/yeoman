// Copyright 2017 Google Inc. All Rights Reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
//
// This is adapted from https://github.com/kelseyhightower/gcscache/, which is
// MIT-licensed.
package google

import (
	"context"
	"errors"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
	"golang.org/x/crypto/acme/autocert"
)

// Cache implements the autocert.Cache interface using Google Cloud Storage.
type Cache struct {
	client *storage.Client
	bucket string
}

// NewCache creates and initializes a new Cache backed by the given Google
// Cloud Storage bucket.
func NewCache(ctx context.Context, bucket string) (*Cache, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	return &Cache{client, bucket}, nil
}

// Get reads a certificate data from the specified object name.
func (c *Cache) Get(ctx context.Context, name string) ([]byte, error) {
	r, err := c.client.Bucket(c.bucket).Object(name).NewReader(ctx)
	switch {
	case errors.Is(err, storage.ErrObjectNotExist):
		return nil, autocert.ErrCacheMiss
	case err != nil:
		return nil, fmt.Errorf("new reader: %w", err)
	}
	defer func() { _ = r.Close() }()
	byt, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read all: %w", err)
	}
	return byt, nil
}

// Put writes the certificate data to the specified object name.
func (c *Cache) Put(ctx context.Context, name string, data []byte) error {
	w := c.client.Bucket(c.bucket).Object(name).NewWriter(ctx)
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// Delete removes the specified object name.
func (c *Cache) Delete(ctx context.Context, name string) error {
	o := c.client.Bucket(c.bucket).Object(name)
	err := o.Delete(ctx)
	switch {
	case errors.Is(err, storage.ErrObjectNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("delete: %w", err)
	}
	return nil
}
