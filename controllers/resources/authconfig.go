/*

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

package resources

import (
	"context"
	_ "embed" // needed for go:embed directive
	"sort"
	"strings"

	kservev1beta1 "github.com/kserve/kserve/pkg/apis/serving/v1beta1"
	authorinov1beta2 "github.com/kuadrant/authorino/api/v1beta2"
	"github.com/opendatahub-io/odh-model-controller/controllers/utils"
	"github.com/pkg/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type AuthType string

const (
	UserDefined AuthType = "userdefined"
	Anonymous   AuthType = "anonymous"
)

type InferenceServiceHostExtractor interface {
	Extract(isvc *kservev1beta1.InferenceService) []string
}

type AuthConfigTemplateLoader interface {
	Load(ctx context.Context, authType AuthType, key types.NamespacedName) (authorinov1beta2.AuthConfig, error)
}

type AuthTypeDetector interface {
	Detect(ctx context.Context, isvc *kservev1beta1.InferenceService) (AuthType, error)
}

type AuthConfigStore interface {
	Get(ctx context.Context, key types.NamespacedName) (*authorinov1beta2.AuthConfig, error)
	Remove(ctx context.Context, key types.NamespacedName) error
	Create(ctx context.Context, authConfig *authorinov1beta2.AuthConfig) error
	Update(ctx context.Context, authConfig *authorinov1beta2.AuthConfig) error
}

//go:embed template/authconfig_anonymous.yaml
var authConfigTemplateAnonymous []byte

//go:embed template/authconfig_userdefined.yaml
var authConfigTemplateUserDefined []byte

type staticTemplateLoader struct {
}

func NewStaticTemplateLoader() AuthConfigTemplateLoader {
	return &staticTemplateLoader{}
}

func (s *staticTemplateLoader) Load(ctx context.Context, authType AuthType, key types.NamespacedName) (authorinov1beta2.AuthConfig, error) {
	authConfig := authorinov1beta2.AuthConfig{}
	if authType == UserDefined {
		err := utils.ConvertToStructuredResource(authConfigTemplateUserDefined, &authConfig)
		if err != nil {
			return authConfig, errors.Wrap(err, "could not load UserDefined template")
		}
		return authConfig, nil
	}
	err := utils.ConvertToStructuredResource(authConfigTemplateAnonymous, &authConfig)
	if err != nil {
		return authConfig, errors.Wrap(err, "could not load Anonymous template")
	}
	return authConfig, nil
}

type configMapTemplateLoader struct {
	client   client.Client
	fallback AuthConfigTemplateLoader
}

func NewConfigMapTemplateLoader(client client.Client, fallback AuthConfigTemplateLoader) AuthConfigTemplateLoader {
	return &configMapTemplateLoader{
		client:   client,
		fallback: fallback,
	}
}

func (c *configMapTemplateLoader) Load(ctx context.Context, authType AuthType, key types.NamespacedName) (authorinov1beta2.AuthConfig, error) {
	// TOOD: check "authconfig-template" CM in key.Namespace to see if there is a "spec" to use, construct a AuthConfig object
	// https://issues.redhat.com/browse/RHOAIENG-847

	// else
	return c.fallback.Load(ctx, authType, key)
}

type clientAuthConfigStore struct {
	client client.Client
}

func NewClientAuthConfigStore(client client.Client) AuthConfigStore {
	return &clientAuthConfigStore{
		client: client,
	}
}

func (c *clientAuthConfigStore) Get(ctx context.Context, key types.NamespacedName) (*authorinov1beta2.AuthConfig, error) {
	authConfig := &authorinov1beta2.AuthConfig{
		TypeMeta: v1.TypeMeta{
			APIVersion: "authorino.kuadrant.io/v1beta2",
			Kind:       "AuthConfig",
		},
	}

	err := c.client.Get(ctx, key, authConfig)
	if err != nil {
		return nil, errors.Wrapf(err, "could not GET authconfig %s", key)
	}
	return authConfig, nil
}

func (c *clientAuthConfigStore) Remove(ctx context.Context, key types.NamespacedName) error {
	authConfig := authorinov1beta2.AuthConfig{}
	authConfig.Name = key.Name
	authConfig.Namespace = key.Namespace
	return errors.Wrapf(c.client.Delete(ctx, &authConfig), "could not DELETE authconfig %s", key)
}

func (c *clientAuthConfigStore) Create(ctx context.Context, authConfig *authorinov1beta2.AuthConfig) error {
	return errors.Wrapf(c.client.Create(ctx, authConfig), "could not CREATE authconfig %s/%s", authConfig.Namespace, authConfig.Name)
}

func (c *clientAuthConfigStore) Update(ctx context.Context, authConfig *authorinov1beta2.AuthConfig) error {
	return errors.Wrapf(c.client.Update(ctx, authConfig), "could not UPDATE authconfig %s/%s", authConfig.Namespace, authConfig.Name)
}

type kserveAuthTypeDetector struct {
	client client.Client
}

func NewKServeAuthTypeDetector(client client.Client) AuthTypeDetector {
	return &kserveAuthTypeDetector{
		client: client,
	}
}

func (k *kserveAuthTypeDetector) Detect(ctx context.Context, isvc *kservev1beta1.InferenceService) (AuthType, error) {
	if value, exist := isvc.Annotations["security.opendatahub.io/enable-auth"]; exist {
		if strings.ToLower(value) == "true" {
			return UserDefined, nil
		}
	} else { // backward compat
		if strings.ToLower(isvc.Annotations["enable-auth"]) == "true" {
			return UserDefined, nil
		}
	}
	return Anonymous, nil
}

type kserveInferenceServiceHostExtractor struct {
}

func NewKServeInferenceServiceHostExtractor() InferenceServiceHostExtractor {
	return &kserveInferenceServiceHostExtractor{}
}

func (k *kserveInferenceServiceHostExtractor) Extract(isvc *kservev1beta1.InferenceService) []string {

	hosts := k.findAllURLHosts(isvc)

	for _, host := range hosts {
		if strings.HasSuffix(host, ".svc.cluster.local") {
			hosts = append(hosts, strings.ReplaceAll(host, ".svc.cluster.local", ""))
			hosts = append(hosts, strings.ReplaceAll(host, ".svc.cluster.local", ".svc"))
		}
	}
	sort.Strings(hosts)
	return hosts
}

func (k *kserveInferenceServiceHostExtractor) findAllURLHosts(isvc *kservev1beta1.InferenceService) []string {
	hosts := []string{}

	if isvc.Status.URL != nil {
		hosts = append(hosts, isvc.Status.URL.Host)
	}

	if isvc.Status.Address != nil && isvc.Status.Address.URL != nil {
		hosts = append(hosts, isvc.Status.Address.URL.Host)
	}

	for _, comp := range isvc.Status.Components {
		if comp.Address != nil && comp.Address.URL != nil {
			hosts = append(hosts, comp.Address.URL.Host)
		}
		if comp.URL != nil {
			hosts = append(hosts, comp.URL.Host)
		}
		if comp.GrpcURL != nil {
			hosts = append(hosts, comp.GrpcURL.Host)
		}
		if comp.RestURL != nil {
			hosts = append(hosts, comp.RestURL.Host)
		}
		for _, tt := range comp.Traffic {
			if tt.URL != nil {
				hosts = append(hosts, tt.URL.Host)
			}
		}
	}

	unique := func(in []string) []string {
		m := map[string]bool{}
		for _, v := range in {
			m[v] = true
		}
		k := make([]string, len(m))
		i := 0
		for v := range m {
			k[i] = v
			i++
		}
		return k
	}
	return unique(hosts)
}
