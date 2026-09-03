/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package objectstoretest is an in-memory S3-compatible store with a MinIO
// admin API, for testing what the provisioner does without a store to do it
// to. It implements objectstore.BucketAPI and objectstore.AdminAPI and
// records every bucket, user, policy and quota it was asked for.
package objectstoretest

import (
	"context"
	"fmt"
	"sync"
)

// Store is the fake. Every field is readable by a test after the fact.
type Store struct {
	mu sync.Mutex

	// Buckets by name, each with its versioning, its anonymous-read policy
	// and the objects in it.
	Buckets map[string]*Bucket
	// Users by access key, each holding its secret key and the policies
	// attached to it.
	Users map[string]*User
	// Policies by name, holding the document.
	Policies map[string][]byte
	// Quotas by bucket, in bytes.
	Quotas map[string]uint64

	// NotReady makes every call answer objectstore.ErrNotReady-shaped
	// errors, the way a store that is still starting does.
	NotReady error
	// Refuse makes MakeBucket fail with this error, for a store that
	// refuses.
	Refuse error
	// NoTagging makes Tags and SetTags fail, the way a compatible store
	// that implements no bucket tagging does.
	NoTagging error
}

// Bucket is one bucket as the fake keeps it.
type Bucket struct {
	Versioned  bool
	PublicRead []byte
	Objects    map[string]int
	// Tags is the bucket's tag set, where the store keeps one.
	Tags map[string]string
}

// User is one user as the fake keeps it.
type User struct {
	SecretKey string
	Policies  []string
}

// New is an empty store.
func New() *Store {
	return &Store{
		Buckets:  map[string]*Bucket{},
		Users:    map[string]*User{},
		Policies: map[string][]byte{},
		Quotas:   map[string]uint64{},
	}
}

// Put puts an object in a bucket, so that a test can check it was removed.
func (s *Store) Put(bucket, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Buckets[bucket]
	if !ok {
		b = &Bucket{Objects: map[string]int{}}
		s.Buckets[bucket] = b
	}
	b.Objects[key]++
}

func (s *Store) BucketExists(_ context.Context, bucket string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.NotReady != nil {
		return false, s.NotReady
	}
	_, ok := s.Buckets[bucket]
	return ok, nil
}

func (s *Store) MakeBucket(_ context.Context, bucket, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.NotReady != nil {
		return s.NotReady
	}
	if s.Refuse != nil {
		return s.Refuse
	}
	if _, ok := s.Buckets[bucket]; !ok {
		s.Buckets[bucket] = &Bucket{Objects: map[string]int{}}
	}
	return nil
}

func (s *Store) SetVersioning(_ context.Context, bucket string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Buckets[bucket]
	if !ok {
		return fmt.Errorf("NoSuchBucket: %s", bucket)
	}
	b.Versioned = enabled
	return nil
}

func (s *Store) Versioning(_ context.Context, bucket string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Buckets[bucket]
	if !ok {
		return false, fmt.Errorf("NoSuchBucket: %s", bucket)
	}
	return b.Versioned, nil
}

// Tag records a tag on a bucket, so that a test can set up a store whose
// buckets were tagged by an earlier reconcile.
func (s *Store) Tag(bucket, key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Buckets[bucket]
	if !ok {
		b = &Bucket{Objects: map[string]int{}}
		s.Buckets[bucket] = b
	}
	if b.Tags == nil {
		b.Tags = map[string]string{}
	}
	b.Tags[key] = value
}

func (s *Store) Tags(_ context.Context, bucket string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.NoTagging != nil {
		return nil, s.NoTagging
	}
	b, ok := s.Buckets[bucket]
	if !ok {
		return nil, fmt.Errorf("NoSuchBucket: %s", bucket)
	}
	out := map[string]string{}
	for key, value := range b.Tags {
		out[key] = value
	}
	return out, nil
}

func (s *Store) SetTags(_ context.Context, bucket string, values map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.NoTagging != nil {
		return s.NoTagging
	}
	b, ok := s.Buckets[bucket]
	if !ok {
		return fmt.Errorf("NoSuchBucket: %s", bucket)
	}
	b.Tags = map[string]string{}
	for key, value := range values {
		b.Tags[key] = value
	}
	return nil
}

func (s *Store) SetAnonymousRead(_ context.Context, bucket string, policy []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.Buckets[bucket]
	if !ok {
		return fmt.Errorf("NoSuchBucket: %s", bucket)
	}
	b.PublicRead = policy
	return nil
}

func (s *Store) RemoveAllObjects(_ context.Context, bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.Buckets[bucket]; ok {
		b.Objects = map[string]int{}
	}
	return nil
}

func (s *Store) RemoveBucket(_ context.Context, bucket string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.Buckets[bucket]; ok && len(b.Objects) > 0 {
		return fmt.Errorf("BucketNotEmpty: %s", bucket)
	}
	delete(s.Buckets, bucket)
	return nil
}

func (s *Store) PutUser(_ context.Context, accessKey, secretKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.Users[accessKey]
	if !ok {
		u = &User{}
		s.Users[accessKey] = u
	}
	u.SecretKey = secretKey
	return nil
}

func (s *Store) RemoveUser(_ context.Context, accessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Users, accessKey)
	return nil
}

func (s *Store) PutPolicy(_ context.Context, name string, document []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Policies[name] = document
	return nil
}

func (s *Store) RemovePolicy(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Policies, name)
	return nil
}

func (s *Store) AttachPolicy(_ context.Context, policy, accessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.Users[accessKey]
	if !ok {
		return fmt.Errorf("XMinioAdminNoSuchUser: %s", accessKey)
	}
	if _, ok := s.Policies[policy]; !ok {
		return fmt.Errorf("XMinioAdminNoSuchPolicy: %s", policy)
	}
	for _, have := range u.Policies {
		if have == policy {
			return nil
		}
	}
	u.Policies = append(u.Policies, policy)
	return nil
}

func (s *Store) SetQuota(_ context.Context, bucket string, bytes uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Buckets[bucket]; !ok {
		return fmt.Errorf("NoSuchBucket: %s", bucket)
	}
	s.Quotas[bucket] = bytes
	return nil
}
