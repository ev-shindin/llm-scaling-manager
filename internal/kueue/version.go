package kueue

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ControllerVersion is what is known of the running Kueue controller's version.
type ControllerVersion struct {
	// Version is the release its image tag names, as "v<major>.<minor>.<patch>"
	// with any pre-release suffix; empty when unknown.
	Version string
	// Tested is true when Version is in TestedKueueVersions. False for an
	// unknown version too: check Known first.
	Tested bool
	// Known is false when the controller's Deployment could not be read
	// (Forbidden, absent, another namespace) or its image names no version, as
	// with a digest-only reference.
	Known bool
	// Down is true when the controller's Deployment reports no ready replica,
	// or is gone after a read found it: Kueue's API still answers, but nothing
	// admits, preempts or writes queue status, so the figures stop moving.
	// New borrowing is withheld (ReasonKueueControllerDown) and what is
	// borrowed is frozen. A Deployment that cannot be read (Forbidden), or was
	// never found, is not Down.
	Down bool
}

// forbiddenTTL is how long a Forbidden read of the controller's Deployment is
// kept: RBAC changes rarely, and asking again every refresh is a call for
// nothing. Every other answer is read again next refresh, since the
// controller's readiness can change at any time.
const forbiddenTTL = 10 * time.Minute

// semverTag matches the release a Kueue image tag names: v0.20.0, 0.20.1,
// v0.21.0-rc.1.
var semverTag = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)([-+][0-9A-Za-z.+-]*)?$`)

// ParseControllerVersion returns the release an image reference's tag names,
// normalized to "v<major>.<minor>.<patch>[suffix]", and false when the
// reference has no tag or the tag names no release. A digest after the tag
// (repo:tag@sha256:...) is ignored.
func ParseControllerVersion(image string) (string, bool) {
	ref, _, _ := strings.Cut(image, "@")
	slash := strings.LastIndex(ref, "/")
	colon := strings.LastIndex(ref, ":")
	if colon <= slash {
		return "", false // no tag: a registry port is not one
	}
	m := semverTag.FindStringSubmatch(ref[colon+1:])
	if m == nil {
		return "", false
	}
	return "v" + m[1] + "." + m[2] + "." + m[3] + m[4], true
}

// VersionTested reports whether version, as ParseControllerVersion returns
// it, is a release of a minor in TestedKueueVersions.
func VersionTested(version string) bool {
	m := semverTag.FindStringSubmatch(version)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return slices.Contains(TestedKueueVersions, fmt.Sprintf("v%d.%d", major, minor))
}

// controllerVersion reads the Kueue controller's version from its Deployment's
// image, and whether it is down, on every read (a Forbidden answer is kept for
// forbiddenTTL). Forbidden, a Deployment never found and a kind the cluster
// does not serve read as unknown and not down. A Deployment with no ready
// replica, or one gone after a read found it, is down. Any other failure keeps
// the last answer and is asked again next read. Callers must hold r.mu.
func (r *Reader) controllerVersion(ctx context.Context) ControllerVersion {
	now := r.opts.now()
	if r.versionForbidden && now.Sub(r.versionAt) < forbiddenTTL {
		return r.version
	}
	ns := r.opts.ControllerNamespace
	if ns == "" {
		ns = DefaultControllerNamespace
	}
	d := &unstructured.Unstructured{}
	d.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	err := r.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: ControllerDeployment}, d)
	var v ControllerVersion
	switch {
	case err == nil:
		v = versionOf(d)
		ready, _, _ := unstructured.NestedInt64(d.Object, "status", "readyReplicas")
		v.Down = ready < 1
		r.controllerSeen = true
	case apierrors.IsNotFound(err) && r.controllerSeen:
		// Gone while the Kueue CRDs are still served: nothing admits. The
		// version last read stays what it was.
		v = r.version
		v.Down = true
	case apierrors.IsForbidden(err), apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		// Unknown until someone acts.
	default:
		return r.version // may pass: asked again next read
	}
	if v.Down != r.version.Down {
		logger := kueueLogger(ctx)
		if v.Down {
			logger.Info("The Kueue controller has no ready replica; new cohort borrowing is withheld (reason kueue-controller-down) and what is borrowed is frozen until it is back",
				"deployment", ns+"/"+ControllerDeployment)
		} else {
			logger.Info("The Kueue controller is ready again; cohort borrowing is counted as before",
				"deployment", ns+"/"+ControllerDeployment)
		}
	}
	vers := func(c ControllerVersion) ControllerVersion { c.Down = false; return c }
	if vers(v) != vers(r.version) || r.versionAt.IsZero() {
		logger := kueueLogger(ctx)
		switch {
		case !v.Known:
			msg := "Cannot tell the Kueue controller's version; borrowing is counted as if it were tested. Logged when the answer changes."
			if err != nil {
				logger.Info(msg, "deployment", ns+"/"+ControllerDeployment, "error", err.Error())
			} else {
				logger.Info(msg, "deployment", ns+"/"+ControllerDeployment)
			}
		case !v.Tested:
			logger.Info("The Kueue controller runs a version outside the tested range; cohort borrowing is withheld (reason kueue-version-untested) and the nominal quotas still apply",
				"version", v.Version, "tested", TestedKueueVersions)
		default:
			logger.V(1).Info("Kueue controller version", "version", v.Version)
		}
	}
	r.version, r.versionAt, r.versionForbidden = v, now, apierrors.IsForbidden(err)
	return v
}

// versionOf reads the version from a Kueue controller Deployment: the image of
// its container named manager, or of its only container.
func versionOf(d *unstructured.Unstructured) ControllerVersion {
	containers, _, _ := unstructured.NestedSlice(d.Object, "spec", "template", "spec", "containers")
	var image string
	for _, raw := range containers {
		c, _ := raw.(map[string]interface{})
		name, _, _ := unstructured.NestedString(c, "name")
		img, _, _ := unstructured.NestedString(c, "image")
		if name == "manager" || len(containers) == 1 {
			image = img
			break
		}
	}
	version, ok := ParseControllerVersion(image)
	if !ok {
		return ControllerVersion{}
	}
	return ControllerVersion{Version: version, Tested: VersionTested(version), Known: true}
}
