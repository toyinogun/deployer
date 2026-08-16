package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The connectors themselves, pinned the way tunnel_test.go pins their routes.
//
// tunnel_test.go covers what the tunnel publishes; this file covers what runs it.
// Neither the replica count nor the shape of the credential is composed by any Go
// code, and both are the kind of edit that reads as harmless in review: one
// replica still serves every request until the node holding it drains, and a
// plain Secret still starts the pod.
const (
	tunnelDeploymentFile   = "../../deploy/cloudflared-deployment.yaml"
	tunnelSealedSecretFile = "../../deploy/cloudflared-sealedsecret.yaml"
	tunnelNamespace        = "deployer-edge"
)

// yamlDocs splits a multi document manifest the same way tunnelPolicies does.
func yamlDocs(t *testing.T, path string) []string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var out []string
	for _, doc := range strings.Split(string(raw), "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		out = append(out, doc)
	}
	return out
}

// docKind reports the kind of one parsed YAML document.
func docKind(t *testing.T, path, doc string) string {
	t.Helper()

	var kind struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal([]byte(doc), &kind); err != nil {
		t.Fatalf("parsing a document of %s: %v", path, err)
	}
	return kind.Kind
}

// tunnelDeployment reads the cloudflared Deployment out of its manifest.
func tunnelDeployment(t *testing.T) appsv1.Deployment {
	t.Helper()

	for _, doc := range yamlDocs(t, tunnelDeploymentFile) {
		if docKind(t, tunnelDeploymentFile, doc) != "Deployment" {
			continue
		}
		var d appsv1.Deployment
		if err := yaml.Unmarshal([]byte(doc), &d); err != nil {
			t.Fatalf("parsing the Deployment in %s: %v", tunnelDeploymentFile, err)
		}
		return d
	}
	t.Fatalf("%s holds no Deployment", tunnelDeploymentFile)
	return appsv1.Deployment{}
}

// AC-10: two replicas, so a node drain or a rolling update never leaves zero
// connectors.
//
// covers: AC-10
//
// This is the one place in this repository where a single replica is wrong.
// deployment.yaml holds to exactly one on purpose, because two writers on one
// SQLite file is corruption, and that reasoning does not carry over: cloudflared
// dials out and holds the connection open, so a replica is a connector rather
// than a backend and Cloudflare balances across whichever have registered.
//
// The suite cannot see the replicas actually running, which is what
// `kubectl -n deployer-edge get deploy cloudflared` in verify.md is for. What it
// can see is the number a reviewer would have to change deliberately.
func TestTheTunnelRunsTwoConnectors(t *testing.T) {
	d := tunnelDeployment(t)

	if d.Namespace != tunnelNamespace {
		t.Errorf("the connectors run in namespace %q, want %s: that name is what the control plane's fence "+
			"and the tunnel's own policy both select on", d.Namespace, tunnelNamespace)
	}
	if d.Spec.Replicas == nil {
		t.Fatal("the Deployment names no replica count, so it defaults to one and a node drain takes the " +
			"whole public edge down")
	}
	if got := *d.Spec.Replicas; got < 2 {
		t.Errorf("replicas = %d, want at least 2: one connector means one node drain is a public outage", got)
	}
}

// AC-10: the tunnel credential exists in the cluster only as a SealedSecret and
// appears in plain text in no repository.
//
// covers: AC-10
//
// Two halves, and the second is the one worth having. That the SealedSecret
// exists proves nothing on its own, because a plain Secret alongside it would
// still start the pod and would still be the thing the kubelet mounted. So this
// walks every manifest in deploy/ and fails on any `kind: Secret` at all: every
// secret value in this directory is already sealed, so the rule to hold is the
// directory wide one rather than a check aimed at one key name that a rename
// would slip past.
func TestTheTunnelCredentialIsSealedAndNeverPlainText(t *testing.T) {
	d := tunnelDeployment(t)

	// The Deployment mounts a Secret, and the name it mounts is the name the
	// SealedSecret produces. These live in two files, so a rename in one is a pod
	// that never starts and a suite that never notices.
	var mounted string
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Secret != nil {
			mounted = v.Secret.SecretName
		}
	}
	if mounted == "" {
		t.Fatal("the connector mounts no Secret, so it holds no tunnel credential and registers nothing")
	}

	var sealed struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			EncryptedData map[string]string `json:"encryptedData"`
			Template      struct {
				Data       map[string]string `json:"data"`
				StringData map[string]string `json:"stringData"`
			} `json:"template"`
		} `json:"spec"`
	}
	raw, err := os.ReadFile(tunnelSealedSecretFile)
	if err != nil {
		t.Fatalf("reading %s: %v", tunnelSealedSecretFile, err)
	}
	if err := yaml.Unmarshal(raw, &sealed); err != nil {
		t.Fatalf("parsing %s: %v", tunnelSealedSecretFile, err)
	}
	if sealed.Kind != "SealedSecret" {
		t.Errorf("%s is a %s, want a SealedSecret", tunnelSealedSecretFile, sealed.Kind)
	}
	if sealed.Metadata.Name != mounted {
		t.Errorf("the connector mounts Secret %q but the SealedSecret produces %q, so the pod never starts",
			mounted, sealed.Metadata.Name)
	}
	if sealed.Metadata.Namespace != tunnelNamespace {
		t.Errorf("the SealedSecret is in namespace %q, want %s: a Secret is namespaced and the connector "+
			"cannot mount one from elsewhere", sealed.Metadata.Namespace, tunnelNamespace)
	}
	if len(sealed.Spec.EncryptedData) == 0 {
		t.Error("the SealedSecret carries no encryptedData, so whatever it produces is not the credential")
	}
	// A SealedSecret's template is copied into the Secret it produces verbatim,
	// so a value put there is plain text wearing the right kind.
	if len(sealed.Spec.Template.Data) != 0 || len(sealed.Spec.Template.StringData) != 0 {
		t.Error("the SealedSecret's template carries data, which is copied through in plain text and " +
			"defeats the sealing entirely")
	}

	// And nothing anywhere in deploy/ is a plain Secret.
	entries, err := os.ReadDir("../../deploy")
	if err != nil {
		t.Fatalf("reading deploy/: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join("../../deploy", e.Name())
		for _, doc := range yamlDocs(t, path) {
			if docKind(t, path, doc) == "Secret" {
				t.Errorf("%s holds a plain Secret: every secret value in this directory is sealed, and one "+
					"that is not is a credential committed in the clear", e.Name())
			}
		}
	}
}

// AC-10: the connector image is pinned by digest, never by a mutable tag.
//
// covers: AC-10
//
// The project wide rule, and this image is the awkward one: CI rewrites the
// control plane's digest on every push to main and leaves this one alone,
// because it is third party and bumping it is a deliberate edit with a digest
// read off the registry by hand. Nothing else would notice a tag creeping back
// in here, and a tag is what makes two connectors able to run two different
// builds.
func TestTheTunnelImageIsPinnedByDigest(t *testing.T) {
	containers := tunnelDeployment(t).Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("the connector pod holds %d containers, want exactly 1", len(containers))
	}
	image := containers[0].Image
	if !strings.Contains(image, "@sha256:") {
		t.Errorf("the connector image is %q, want a digest pin: a mutable tag lets the two replicas run "+
			"two different builds and lets an image change under a reviewed manifest", image)
	}
}

// AC-23: the health Service the platform reads through, paired against the port
// the tunnel's own policy permits.
//
// covers: AC-23
//
// Three files have to agree for the tunnel health check to work at all: the
// container's metrics port, the Service in front of it, and the ingress rule in
// cloudflared-networkpolicy.yaml that lets the control plane reach it. They are
// pinned separately elsewhere, so a mismatch between them reads as every file
// being individually correct. The failure it produces is the worst shape
// available: the watcher cannot reach the endpoint, treats that as an outage,
// and mails about a tunnel that is fine.
func TestTheHealthServiceAgreesWithThePortThePolicyPermits(t *testing.T) {
	d := tunnelDeployment(t)

	ports := d.Spec.Template.Spec.Containers[0].Ports
	if len(ports) != 1 {
		t.Fatalf("the connector declares %d ports, want exactly 1 (metrics)", len(ports))
	}
	metrics := ports[0].ContainerPort

	var svc corev1.Service
	var found bool
	for _, doc := range yamlDocs(t, tunnelDeploymentFile) {
		if docKind(t, tunnelDeploymentFile, doc) != "Service" {
			continue
		}
		if err := yaml.Unmarshal([]byte(doc), &svc); err != nil {
			t.Fatalf("parsing the Service in %s: %v", tunnelDeploymentFile, err)
		}
		found = true
	}
	if !found {
		t.Fatal("there is no health Service, so the platform has no way to read the ready endpoint without " +
			"a Kubernetes API call and a new Role")
	}
	if svc.Namespace != tunnelNamespace {
		t.Errorf("the health Service is in namespace %q, want %s", svc.Namespace, tunnelNamespace)
	}
	if len(svc.Spec.Ports) != 1 || svc.Spec.Ports[0].Port != metrics {
		t.Errorf("the health Service publishes %+v, want port %d, the container's metrics port",
			svc.Spec.Ports, metrics)
	}

	// The policy has to permit that same port, or the read is refused rather
	// than answered and a healthy tunnel is reported as down.
	rule := tunnelPolicies(t)["cloudflared-allow"].Spec.Ingress[0]
	assertPorts(t, "the tunnel health check", rule.Ports,
		map[corev1.Protocol][]int32{corev1.ProtocolTCP: {metrics}})
}
