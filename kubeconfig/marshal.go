package kubeconfig

import (
	"encoding/json"
	"fmt"

	"github.com/krateo-platformops/plumbing/endpoints"
)

const (
	defaultClusterName = "krateo"
)

func Marshal(ep *endpoints.Endpoint) ([]byte, error) {
	kc := KubeConfig{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters: Clusters{
			0: {
				Cluster: ClusterInfo{
					CertificateAuthorityData: ep.CertificateAuthorityData,
					ProxyURL:                 ep.ProxyURL,
					Server:                   ep.ServerURL,
				},
				Name: defaultClusterName,
			},
		},
		Contexts: Contexts{
			0: {
				Context: Context{
					Cluster: defaultClusterName,
					User:    ep.Username,
				},
				Name: defaultClusterName,
			},
		},
		CurrentContext: defaultClusterName,
		Users: Users{
			0: {
				CertInfo: CertInfo{
					ClientCertificateData: ep.ClientCertificateData,
					ClientKeyData:         ep.ClientKeyData,
					Token:                 ep.Token,
				},
				Name: ep.Username,
			},
		},
	}

	out, err := json.Marshal(kc)
	if err != nil {
		return nil, fmt.Errorf("generating kubeconfig from endpoint: %w", err)
	}

	return out, nil
}
