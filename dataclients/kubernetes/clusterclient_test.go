package kubernetes

import (
	"bytes"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	log "github.com/sirupsen/logrus"
)

func containsCount(s, substr string, count int) bool {
	var found int
	for {
		i := strings.Index(s, substr)
		if i < 0 {
			return found == count
		}

		if found == count {
			return false
		}

		found++
		s = s[i+len(substr):]
	}
}

func containsEveryLineCount(s, substr string, count int) bool {
	l := strings.Split(substr, "\n")
	for _, li := range l {
		if !containsCount(s, li, count) {
			return false
		}
	}

	return true
}

func TestMissingRouteGroupsCRDLoggedOnlyOnce(t *testing.T) {
	a, err := newAPI(testAPIOptions{FindNot: []string{clusterZalandoResourcesURI}})
	if err != nil {
		t.Fatal(err)
	}

	s := httptest.NewServer(a)
	defer s.Close()

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	c, err := New(Options{KubernetesURL: s.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if _, err := c.LoadAll(); err != nil {
		t.Fatal(err)
	}

	if _, err := c.LoadAll(); err != nil {
		t.Fatal(err)
	}

	if !containsEveryLineCount(logBuf.String(), routeGroupsNotInstalledMessage, 1) {
		t.Error("missing RouteGroups CRD was not reported exactly once")
	}
}

func TestLoadRouteGroups(t *testing.T) {

	for _, tt := range []struct {
		msg     string
		rgClass string
		spec    string
		loads   bool
	}{{
		msg:     "annotation set, and matches class",
		rgClass: "test",
		spec: `
apiVersion: zalando.org/v1
kind: RouteGroup
metadata:
  name: foo
  annotations:
    zalando.org/routegroup.class: test
spec:
  hosts:
  - foo.example.org
  backends:
  - name: foo
    type: service
    serviceName: foo
    servicePort: 80
  routes:
  - pathSubtree: /
    backends:
    - backendName: foo
`,
		loads: true,
	}, {
		msg:     "annotation set, and class doesn't match",
		rgClass: "test",
		spec: `
apiVersion: zalando.org/v1
kind: RouteGroup
metadata:
  name: foo
  annotations:
    zalando.org/routegroup.class: incorrectclass
spec:
  hosts:
  - foo.example.org
  backends:
  - name: foo
    type: service
    serviceName: foo
    servicePort: 80
  routes:
  - pathSubtree: /
    backends:
    - backendName: foo
`,
		loads: false,
	}, {
		msg:     "no annotation is loaded",
		rgClass: "test",
		spec: `
apiVersion: zalando.org/v1
kind: RouteGroup
metadata:
  name: foo
  annotations: {}
spec:
  hosts:
  - foo.example.org
  backends:
  - name: foo
    type: service
    serviceName: foo
    servicePort: 80
  routes:
  - pathSubtree: /
    backends:
    - backendName: foo
`,
		loads: true,
	}, {
		msg:     "empty annotation is loaded",
		rgClass: "test",
		spec: `
apiVersion: zalando.org/v1
kind: RouteGroup
metadata:
  name: foo
  annotations:
    zalando.org/routegroup.class: ""
spec:
  hosts:
  - foo.example.org
  backends:
  - name: foo
    type: service
    serviceName: foo
    servicePort: 80
  routes:
  - pathSubtree: /
    backends:
    - backendName: foo
`,
		loads: true,
	}, {
		msg:     "annotation matches regexp class, route group loads",
		rgClass: "^test.*",
		spec: `
apiVersion: zalando.org/v1
kind: RouteGroup
metadata:
  name: foo
  annotations:
    zalando.org/routegroup.class: testing
spec:
  hosts:
  - foo.example.org
  backends:
  - name: foo
    type: service
    serviceName: foo
    servicePort: 80
  routes:
  - pathSubtree: /
    backends:
    - backendName: foo
`,
		loads: true,
	}, {
		msg:     "annotation doesn't matches regexp class, route group isn't loaded",
		rgClass: "^test.*",
		spec: `
apiVersion: zalando.org/v1
kind: RouteGroup
metadata:
  name: foo
  annotations:
    zalando.org/routegroup.class: a-test
spec:
  hosts:
  - foo.example.org
  backends:
  - name: foo
    type: service
    serviceName: foo
    servicePort: 80
  routes:
  - pathSubtree: /
    backends:
    - backendName: foo
`,
		loads: false,
	}} {

		t.Run(tt.msg, func(t *testing.T) {
			a, err := newAPI(testAPIOptions{}, bytes.NewBufferString(tt.spec))
			if err != nil {
				t.Error(err)
			}

			s := httptest.NewServer(a)
			defer s.Close()

			c, err := New(Options{KubernetesURL: s.URL, RouteGroupClass: tt.rgClass})
			if err != nil {
				t.Error(err)
			}
			defer c.Close()

			rgs, err := c.clusterClient.loadRouteGroups()
			if err != nil {
				t.Error(err)
			}

			if tt.loads != (len(rgs) == 1) {
				t.Errorf("mismatch when loading route groups. Expected loads: %t, actual %t", tt.loads, (len(rgs) == 1))
			}
		})
	}
}
