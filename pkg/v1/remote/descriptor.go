// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/internal/redact"
	"github.com/google/go-containerregistry/internal/verify"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/logs"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

var allManifestMediaTypes = append(append([]types.MediaType{
	types.DockerManifestSchema1,
	types.DockerManifestSchema1Signed,
}, acceptableImageMediaTypes...), acceptableIndexMediaTypes...)

// ErrSchema1 indicates that we received a schema1 manifest from the registry.
// This library doesn't have plans to support this legacy image format:
// https://github.com/google/go-containerregistry/issues/377
type ErrSchema1 struct {
	schema string
}

// newErrSchema1 returns an ErrSchema1 with the unexpected MediaType.
func newErrSchema1(schema types.MediaType) error {
	return &ErrSchema1{
		schema: string(schema),
	}
}

// Error implements error.
func (e *ErrSchema1) Error() string {
	return fmt.Sprintf("unsupported MediaType: %q, see https://github.com/google/go-containerregistry/issues/377", e.schema)
}

// Descriptor provides access to metadata about remote artifact and accessors
// for efficiently converting it into a v1.Image or v1.ImageIndex.
type Descriptor struct {
	fetcher
	v1.Descriptor

	ref      name.Reference
	Manifest []byte

	// So we can share this implementation with Image.
	platform v1.Platform
}

func (d *Descriptor) toDesc() v1.Descriptor {
	return d.Descriptor
}

// RawManifest exists to satisfy the Taggable interface.
func (d *Descriptor) RawManifest() ([]byte, error) {
	return d.Manifest, nil
}

// Get returns a remote.Descriptor for the given reference. The response from
// the registry is left un-interpreted, for the most part. This is useful for
// querying what kind of artifact a reference represents.
//
// See Head if you don't need the response body.
func Get(ref name.Reference, options ...Option) (*Descriptor, error) {
	return get(ref, allManifestMediaTypes, options...)
}

// Head returns a v1.Descriptor for the given reference by issuing a HEAD
// request.
//
// Note that the server response will not have a body, so any errors encountered
// should be retried with Get to get more details.
func Head(ref name.Reference, options ...Option) (*v1.Descriptor, error) {
	o, err := makeOptions(options...)
	if err != nil {
		return nil, err
	}

	f, err := makeFetcher(o.context, ref.Context(), o)
	if err != nil {
		return nil, err
	}

	return f.headManifest(o.context, ref, allManifestMediaTypes)
}

// Handle options and fetch the manifest with the acceptable MediaTypes in the
// Accept header.
func get(ref name.Reference, acceptable []types.MediaType, options ...Option) (*Descriptor, error) {
	o, err := makeOptions(options...)
	if err != nil {
		return nil, err
	}
	f, err := makeFetcher(o.context, ref.Context(), o)
	if err != nil {
		return nil, err
	}
	return f.get(o.context, ref, acceptable)
}

func (f *fetcher) get(ctx context.Context, ref name.Reference, acceptable []types.MediaType) (*Descriptor, error) {
	b, desc, err := f.fetchManifest(ctx, ref, acceptable)
	if err != nil {
		return nil, err
	}
	return &Descriptor{
		fetcher:    *f,
		ref:        ref,
		Manifest:   b,
		Descriptor: *desc,
		platform:   f.platform,
	}, nil
}

// Image converts the Descriptor into a v1.Image.
//
// If the fetched artifact is already an image, it will just return it.
//
// If the fetched artifact is an index, it will attempt to resolve the index to
// a child image with the appropriate platform.
//
// See WithPlatform to set the desired platform.
func (d *Descriptor) Image() (v1.Image, error) {
	switch d.MediaType {
	case types.DockerManifestSchema1, types.DockerManifestSchema1Signed:
		// We don't care to support schema 1 images:
		// https://github.com/google/go-containerregistry/issues/377
		return nil, newErrSchema1(d.MediaType)
	case types.OCIImageIndex, types.DockerManifestList:
		// We want an image but the registry has an index, resolve it to an image.
		return d.remoteIndex().imageByPlatform(d.platform)
	case types.OCIManifestSchema1, types.DockerManifestSchema2:
		// These are expected. Enumerated here to allow a default case.
	default:
		// We could just return an error here, but some registries (e.g. static
		// registries) don't set the Content-Type headers correctly, so instead...
		logs.Warn.Printf("Unexpected media type for Image(): %s", d.MediaType)
	}

	// Wrap the v1.Layers returned by this v1.Image in a hint for downstream
	// remote.Write calls to facilitate cross-repo "mounting".
	imgCore, err := partial.CompressedToImage(d.remoteImage())
	if err != nil {
		return nil, err
	}
	return &mountableImage{
		Image:     imgCore,
		Reference: d.ref,
	}, nil
}

// Schema1 converts the Descriptor into a v1.Image for v2 schema 1 media types.
//
// The v1.Image returned by this method does not implement the entire interface because it would be inefficient.
// This exists mostly to make it easier to copy schema 1 images around or look at their filesystems.
// This is separate from Image() to avoid a backward incompatible change for callers expecting ErrSchema1.
func (d *Descriptor) Schema1() (v1.Image, error) {
	i := &schema1{
		fetcher:    d.fetcher,
		ref:        d.ref,
		manifest:   d.Manifest,
		mediaType:  d.MediaType,
		descriptor: &d.Descriptor,
	}

	return &mountableImage{
		Image:     i,
		Reference: d.ref,
	}, nil
}

// ImageIndex converts the Descriptor into a v1.ImageIndex.
func (d *Descriptor) ImageIndex() (v1.ImageIndex, error) {
	switch d.MediaType {
	case types.DockerManifestSchema1, types.DockerManifestSchema1Signed:
		// We don't care to support schema 1 images:
		// https://github.com/google/go-containerregistry/issues/377
		return nil, newErrSchema1(d.MediaType)
	case types.OCIManifestSchema1, types.DockerManifestSchema2:
		// We want an index but the registry has an image, nothing we can do.
		return nil, fmt.Errorf("unexpected media type for ImageIndex(): %s; call Image() instead", d.MediaType)
	case types.OCIImageIndex, types.DockerManifestList:
		// These are expected.
	default:
		// We could just return an error here, but some registries (e.g. static
		// registries) don't set the Content-Type headers correctly, so instead...
		logs.Warn.Printf("Unexpected media type for ImageIndex(): %s", d.MediaType)
	}
	return d.remoteIndex(), nil
}

func (d *Descriptor) remoteImage() *remoteImage {
	return &remoteImage{
		fetcher:    d.fetcher,
		ref:        d.ref,
		manifest:   d.Manifest,
		mediaType:  d.MediaType,
		descriptor: &d.Descriptor,
	}
}

func (d *Descriptor) remoteIndex() *remoteIndex {
	return &remoteIndex{
		fetcher:    d.fetcher,
		ref:        d.ref,
		manifest:   d.Manifest,
		mediaType:  d.MediaType,
		descriptor: &d.Descriptor,
	}
}

type resource interface {
	Scheme() string
	RegistryStr() string
	Scope(string) string

	authn.Resource
}

// fetcher implements methods for reading from a registry.
type fetcher struct {
	target   resource
	client   *http.Client
	context  context.Context
	platform v1.Platform
}

func makeFetcher(ctx context.Context, target resource, o *options) (*fetcher, error) {
	auth := o.auth
	if o.keychain != nil {
		kauth, err := o.keychain.Resolve(target)
		if err != nil {
			return nil, err
		}
		auth = kauth
	}

	reg, ok := target.(name.Registry)
	if !ok {
		repo, ok := target.(name.Repository)
		if !ok {
			return nil, fmt.Errorf("unexpected resource: %T", target)
		}
		reg = repo.Registry
	}

	tr, err := transport.NewWithContext(ctx, reg, auth, o.transport, []string{target.Scope(transport.PullScope)})
	if err != nil {
		return nil, err
	}
	return &fetcher{
		target:   target,
		client:   &http.Client{Transport: tr},
		context:  ctx,
		platform: o.platform,
	}, nil
}

// url returns a url.Url for the specified path in the context of this remote image reference.
func (f *fetcher) url(resource, identifier string) url.URL {
	u := url.URL{
		Scheme: f.target.Scheme(),
		Host:   f.target.RegistryStr(),
		// Default path if this is not a repository.
		Path: "/v2/_catalog",
	}
	if repo, ok := f.target.(name.Repository); ok {
		u.Path = fmt.Sprintf("/v2/%s/%s/%s", repo.RepositoryStr(), resource, identifier)
	}
	return u
}

// https://github.com/opencontainers/distribution-spec/blob/main/spec.md#referrers-tag-schema
func fallbackTag(d name.Digest) name.Tag {
	return d.Context().Tag(strings.Replace(d.DigestStr(), ":", "-", 1))
}

func (f *fetcher) fetchReferrers(ctx context.Context, filter map[string]string, d name.Digest) (v1.ImageIndex, error) {
	// Check the Referrers API endpoint first.
	u := f.url("referrers", d.DigestStr())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", string(types.OCIImageIndex))

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := transport.CheckError(resp, http.StatusOK, http.StatusNotFound, http.StatusBadRequest); err != nil {
		return nil, err
	}

	var b []byte
	if resp.StatusCode == http.StatusOK {
		b, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
	} else {
		// The registry doesn't support the Referrers API endpoint, so we'll use the fallback tag scheme.
		b, _, err = f.fetchManifest(ctx, fallbackTag(d), []types.MediaType{types.OCIImageIndex})
		var terr *transport.Error
		if errors.As(err, &terr) && terr.StatusCode == http.StatusNotFound {
			// Not found just means there are no attachments yet. Start with an empty manifest.
			return empty.Index, nil
		} else if err != nil {
			return nil, err
		}
	}

	h, sz, err := v1.SHA256(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	idx := &remoteIndex{
		fetcher:   *f,
		manifest:  b,
		mediaType: types.OCIImageIndex,
		descriptor: &v1.Descriptor{
			Digest:    h,
			MediaType: types.OCIImageIndex,
			Size:      sz,
		},
	}
	return filterReferrersResponse(filter, idx), nil
}

func (f *fetcher) fetchManifest(ctx context.Context, ref name.Reference, acceptable []types.MediaType) ([]byte, *v1.Descriptor, error) {
	u := f.url("manifests", ref.Identifier())
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	accept := []string{}
	for _, mt := range acceptable {
		accept = append(accept, string(mt))
	}
	req.Header.Set("Accept", strings.Join(accept, ","))

	resp, err := f.client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if err := transport.CheckError(resp, http.StatusOK); err != nil {
		return nil, nil, err
	}

	manifest, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}

	digest, size, err := v1.SHA256(bytes.NewReader(manifest))
	if err != nil {
		return nil, nil, err
	}

	mediaType := types.MediaType(resp.Header.Get("Content-Type"))
	contentDigest, err := v1.NewHash(resp.Header.Get("Docker-Content-Digest"))
	if err == nil && mediaType == types.DockerManifestSchema1Signed {
		// If we can parse the digest from the header, and it's a signed schema 1
		// manifest, let's use that for the digest to appease older registries.
		digest = contentDigest
	}

	// Validate the digest matches what we asked for, if pulling by digest.
	if dgst, ok := ref.(name.Digest); ok {
		if digest.String() != dgst.DigestStr() {
			return nil, nil, fmt.Errorf("manifest digest: %q does not match requested digest: %q for %q", digest, dgst.DigestStr(), ref)
		}
	}

	var artifactType string
	mf, _ := v1.ParseManifest(bytes.NewReader(manifest))
	// Failing to parse as a manifest should just be ignored.
	// The manifest might not be valid, and that's okay.
	if mf != nil && !mf.Config.MediaType.IsConfig() {
		artifactType = string(mf.Config.MediaType)
	}

	// Do nothing for tags; I give up.
	//
	// We'd like to validate that the "Docker-Content-Digest" header matches what is returned by the registry,
	// but so many registries implement this incorrectly that it's not worth checking.
	//
	// For reference:
	// https://github.com/GoogleContainerTools/kaniko/issues/298

	// Return all this info since we have to calculate it anyway.
	desc := v1.Descriptor{
		Digest:       digest,
		Size:         size,
		MediaType:    mediaType,
		ArtifactType: artifactType,
	}

	return manifest, &desc, nil
}

func (f *fetcher) headManifest(ctx context.Context, ref name.Reference, acceptable []types.MediaType) (*v1.Descriptor, error) {
	u := f.url("manifests", ref.Identifier())
	req, err := http.NewRequest(http.MethodHead, u.String(), nil)
	if err != nil {
		return nil, err
	}
	accept := []string{}
	for _, mt := range acceptable {
		accept = append(accept, string(mt))
	}
	req.Header.Set("Accept", strings.Join(accept, ","))

	resp, err := f.client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := transport.CheckError(resp, http.StatusOK); err != nil {
		return nil, err
	}

	mth := resp.Header.Get("Content-Type")
	if mth == "" {
		return nil, fmt.Errorf("HEAD %s: response did not include Content-Type header", u.String())
	}
	mediaType := types.MediaType(mth)

	size := resp.ContentLength
	if size == -1 {
		return nil, fmt.Errorf("GET %s: response did not include Content-Length header", u.String())
	}

	dh := resp.Header.Get("Docker-Content-Digest")
	if dh == "" {
		return nil, fmt.Errorf("HEAD %s: response did not include Docker-Content-Digest header", u.String())
	}
	digest, err := v1.NewHash(dh)
	if err != nil {
		return nil, err
	}

	// Validate the digest matches what we asked for, if pulling by digest.
	if dgst, ok := ref.(name.Digest); ok {
		if digest.String() != dgst.DigestStr() {
			return nil, fmt.Errorf("manifest digest: %q does not match requested digest: %q for %q", digest, dgst.DigestStr(), ref)
		}
	}

	// Return all this info since we have to calculate it anyway.
	return &v1.Descriptor{
		Digest:    digest,
		Size:      size,
		MediaType: mediaType,
	}, nil
}

func (f *fetcher) fetchBlob(ctx context.Context, size int64, h v1.Hash) (io.ReadCloser, error) {
	u := f.url("blobs", h.String())
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := f.client.Do(req.WithContext(ctx))
	if err != nil {
		return nil, redact.Error(err)
	}

	if err := transport.CheckError(resp, http.StatusOK); err != nil {
		resp.Body.Close()
		return nil, err
	}

	// Do whatever we can.
	// If we have an expected size and Content-Length doesn't match, return an error.
	// If we don't have an expected size and we do have a Content-Length, use Content-Length.
	if hsize := resp.ContentLength; hsize != -1 {
		if size == verify.SizeUnknown {
			size = hsize
		} else if hsize != size {
			return nil, fmt.Errorf("GET %s: Content-Length header %d does not match expected size %d", u.String(), hsize, size)
		}
	}

	return verify.ReadCloser(resp.Body, size, h)
}

func (f *fetcher) headBlob(h v1.Hash) (*http.Response, error) {
	u := f.url("blobs", h.String())
	req, err := http.NewRequest(http.MethodHead, u.String(), nil)
	if err != nil {
		return nil, err
	}

	resp, err := f.client.Do(req.WithContext(f.context))
	if err != nil {
		return nil, redact.Error(err)
	}

	if err := transport.CheckError(resp, http.StatusOK); err != nil {
		resp.Body.Close()
		return nil, err
	}

	return resp, nil
}

func (f *fetcher) blobExists(h v1.Hash) (bool, error) {
	u := f.url("blobs", h.String())
	req, err := http.NewRequest(http.MethodHead, u.String(), nil)
	if err != nil {
		return false, err
	}

	resp, err := f.client.Do(req.WithContext(f.context))
	if err != nil {
		return false, redact.Error(err)
	}
	defer resp.Body.Close()

	if err := transport.CheckError(resp, http.StatusOK, http.StatusNotFound); err != nil {
		return false, err
	}

	return resp.StatusCode == http.StatusOK, nil
}

// If filter applied, filter out by artifactType.
// See https://github.com/opencontainers/distribution-spec/blob/main/spec.md#listing-referrers
func filterReferrersResponse(filter map[string]string, in v1.ImageIndex) v1.ImageIndex {
	if filter == nil {
		return in
	}
	v, ok := filter["artifactType"]
	if !ok {
		return in
	}
	return mutate.RemoveManifests(in, func(desc v1.Descriptor) bool {
		return desc.ArtifactType != v
	})
}
