package build

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Credential is the registry login a build pushes with, and the one an app
// namespace pulls with. Both are the same credential: distribution's htpasswd
// auth has no per user access control, so a second one would grant exactly the
// same rights and minting one for pulls would be theatre (spec 0004).
type Credential struct {
	Host     string
	User     string
	Password string
}

// DockerConfigJSON encodes a credential as the docker config a kubelet and the
// Buildpacks lifecycle both understand.
func DockerConfigJSON(c Credential) ([]byte, error) {
	// The `auth` field is what every client actually reads; user and password
	// are written beside it because some readers still expect them.
	entry := map[string]string{
		"username": c.User,
		"password": c.Password,
		"auth":     base64.StdEncoding.EncodeToString([]byte(c.User + ":" + c.Password)),
	}
	encoded, err := json.Marshal(map[string]any{
		"auths": map[string]any{c.Host: entry},
	})
	if err != nil {
		return nil, fmt.Errorf("build: encoding the registry credential: %w", err)
	}
	return encoded, nil
}

// PullSecret composes the dockerconfigjson Secret an app namespace pulls with.
//
// It is referenced from the Deployment's imagePullSecrets and nothing else: the
// kubelet consumes it, and the app container can neither mount it, project it,
// nor read it as an environment variable (spec 0004, AC-12).
func PullSecret(name, namespace string, c Credential) (*corev1.Secret, error) {
	config, err := DockerConfigJSON(c)
	if err != nil {
		return nil, err
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "deployer"},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{corev1.DockerConfigJsonKey: config},
	}, nil
}

// BuildCredentialSecret composes the per Job credential a build pushes with.
//
// The owner reference is the point: Kubernetes collects this secret when the
// Job is reaped, so a write credential never outlives the one build it was for.
// The caller supplies the owner because a Job has no UID until it exists.
func BuildCredentialSecret(deploymentID, namespace string, c Credential, owner metav1.OwnerReference) (*corev1.Secret, error) {
	secret, err := PullSecret(SecretName(deploymentID), namespace, c)
	if err != nil {
		return nil, err
	}
	secret.OwnerReferences = []metav1.OwnerReference{owner}
	return secret, nil
}

// TargetImage is where this deployment's build pushes. The tag exists only so
// the registry has something to name the manifest under: every deploy runs by
// the digest the platform reads back, never by this (spec 0004, Key invariants).
func TargetImage(registryHost, slug, deploymentID string) string {
	return registryHost + "/apps/" + slug + ":" + deploymentID
}

// ImageRepo is the repository half of a target image, which is what a digest is
// resolved and pulled against.
func ImageRepo(registryHost, slug string) string {
	return registryHost + "/apps/" + slug
}
