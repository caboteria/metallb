package main

import (
	"fmt"
	"math/rand"
	"net"
	"testing"

	"go.universe.tf/metallb/internal/k8s"

	"github.com/go-kit/kit/log"
	"github.com/google/go-cmp/cmp"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func diffService(a, b *v1.Service) string {
	// v5 of the k8s client does not correctly compare nil
	// *metav1.Time objects, which svc.ObjectMeta contains. Add
	// some dummy non-nil values to all of in, want, got to work
	// around this until we migrate to v6.
	if a != nil {
		newA := new(v1.Service)
		*newA = *a
		newA.ObjectMeta.DeletionTimestamp = &metav1.Time{}
		a = newA
	}
	if b != nil {
		newB := new(v1.Service)
		*newB = *b
		newB.ObjectMeta.DeletionTimestamp = &metav1.Time{}
		b = newB
	}
	return cmp.Diff(a, b)
}

func ipnet(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

func statusAssigned(ip string) v1.ServiceStatus {
	return v1.ServiceStatus{
		LoadBalancer: v1.LoadBalancerStatus{
			Ingress: []v1.LoadBalancerIngress{
				{
					IP: ip,
				},
			},
		},
	}
}

// testK8S implements service by recording what the controller wants
// to do to k8s.
type testK8S struct {
	updateService       *v1.Service
	updateServiceStatus *v1.ServiceStatus
	loggedWarning       bool
	t                   *testing.T
}

func (s *testK8S) Update(svc *v1.Service) (*v1.Service, error) {
	s.updateService = svc
	return svc, nil
}

func (s *testK8S) UpdateStatus(svc *v1.Service) error {
	s.updateServiceStatus = &svc.Status
	return nil
}

func (s *testK8S) Infof(_ *v1.Service, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Info event %q: %s", evtType, fmt.Sprintf(msg, args...))
}

func (s *testK8S) Errorf(_ *v1.Service, evtType string, msg string, args ...interface{}) {
	s.t.Logf("k8s Warning event %q: %s", evtType, fmt.Sprintf(msg, args...))
	s.loggedWarning = true
}

func (s *testK8S) reset() {
	s.updateService = nil
	s.updateServiceStatus = nil
	s.loggedWarning = false
}

func (s *testK8S) gotService(in *v1.Service) *v1.Service {
	if s.updateService == nil && s.updateServiceStatus == nil {
		return nil
	}

	ret := new(v1.Service)
	if in != nil {
		*ret = *in
	}
	if s.updateService != nil {
		*ret = *s.updateService
	}
	if s.updateServiceStatus != nil {
		ret.Status = *s.updateServiceStatus
	}
	return ret
}

func TestControllerMutation(t *testing.T) {
	k := &testK8S{t: t}
	c := &controller{
		client: k,
	}
	l := log.NewNopLogger()

	// In steady state, every input below should be equivalent to a
	// pure function that reliably produces the same end state
	// regardless of past controller state.
	tests := []*struct {
		desc    string
		in      *v1.Service
		want    *v1.Service
		wantErr bool
	}{
		{
			desc: "deleted balancer",
		},

		{
			desc: "simple non-LoadBalancer",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "ClusterIP",
					ClusterIP: "1.2.3.4",
				},
			},
		},

		{
			desc: "simple LoadBalancer",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "LoadBalancer",
					ClusterIP: "1.2.3.4",
				},
			},
			want: &v1.Service{
				Spec: v1.ServiceSpec{
					ClusterIP: "1.2.3.4",
					Type:      "LoadBalancer",
				},
				Status: statusAssigned("1.2.3.0"),
			},
		},

		{
			desc: "request specific IP",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					ClusterIP:      "1.2.3.4",
					Type:           "LoadBalancer",
					LoadBalancerIP: "1.2.3.1",
				},
			},
			want: &v1.Service{
				Spec: v1.ServiceSpec{
					ClusterIP:      "1.2.3.4",
					Type:           "LoadBalancer",
					LoadBalancerIP: "1.2.3.1",
				},
				Status: statusAssigned("1.2.3.1"),
			},
		},

		{
			desc: "request invalid IP",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:           "LoadBalancer",
					LoadBalancerIP: "please sir may I have an IP address thank you",
					ClusterIP:      "1.2.3.4",
				},
			},
			wantErr: true,
		},

		{
			desc: "request infeasible IP",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:           "LoadBalancer",
					LoadBalancerIP: "1.2.3.4",
					ClusterIP:      "1.2.3.4",
				},
				Status: statusAssigned("1.2.3.1"),
			},
			want: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:           "LoadBalancer",
					LoadBalancerIP: "1.2.3.4",
					ClusterIP:      "1.2.3.4",
				},
			},
			wantErr: true,
		},

		{
			desc: "request IP from specific pool",
			in: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"metallb.universe.tf/address-pool": "pool1",
					},
				},
				Spec: v1.ServiceSpec{
					Type:      "LoadBalancer",
					ClusterIP: "1.2.3.4",
				},
			},
			want: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"metallb.universe.tf/address-pool": "pool1",
					},
				},
				Spec: v1.ServiceSpec{
					Type:      "LoadBalancer",
					ClusterIP: "1.2.3.4",
				},
				Status: statusAssigned("1.2.3.0"),
			},
		},

		{
			desc: "switch to a different specific pool",
			in: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"metallb.universe.tf/address-pool": "pool2",
					},
				},
				Spec: v1.ServiceSpec{
					ClusterIP: "1.2.3.4",
					Type:      "LoadBalancer",
				},
				Status: statusAssigned("1.2.3.0"),
			},
			want: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"metallb.universe.tf/address-pool": "pool2",
					},
				},
				Spec: v1.ServiceSpec{
					ClusterIP: "1.2.3.4",
					Type:      "LoadBalancer",
				},
				Status: statusAssigned("3.4.5.6"),
			},
		},

		{
			desc: "unknown pool requested",
			in: &v1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"metallb.universe.tf/address-pool": "does-not-exist",
					},
				},
				Spec: v1.ServiceSpec{
					ClusterIP: "1.2.3.4",
					Type:      "LoadBalancer",
				},
			},
			wantErr: true,
		},

		{
			desc: "invalid IP assigned",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "LoadBalancer",
					ClusterIP: "1.2.3.4",
				},
				Status: statusAssigned("2.3.4.5"),
			},
			want: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "LoadBalancer",
					ClusterIP: "1.2.3.4",
				},
				Status: statusAssigned("1.2.3.0"),
			},
		},

		{
			desc: "invalid ingress state",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "LoadBalancer",
					ClusterIP: "1.2.3.4",
				},
				Status: v1.ServiceStatus{
					LoadBalancer: v1.LoadBalancerStatus{
						Ingress: []v1.LoadBalancerIngress{
							{
								Hostname: "foo.bar.local",
							},
							{
								IP: "10.10.10.10",
							},
						},
					},
				},
			},
			want: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "LoadBalancer",
					ClusterIP: "1.2.3.4",
				},
				Status: statusAssigned("1.2.3.0"),
			},
		},

		{
			desc: "former LoadBalancer, now NodePort",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "NodePort",
					ClusterIP: "1.2.3.4",
				},
				Status: statusAssigned("1.2.3.0"),
			},
			want: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "NodePort",
					ClusterIP: "1.2.3.4",
				},
			},
		},

		{
			desc: "request layer2 service",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:           "LoadBalancer",
					LoadBalancerIP: "3.4.5.6",
					ClusterIP:      "1.2.3.4",
				},
			},
			want: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:           "LoadBalancer",
					LoadBalancerIP: "3.4.5.6",
					ClusterIP:      "1.2.3.4",
				},
				Status: statusAssigned("3.4.5.6"),
			},
		},

		{
			desc: "Layer2 service with local traffic policy",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					LoadBalancerIP:        "3.4.5.6",
					ExternalTrafficPolicy: "Local",
					ClusterIP:             "1.2.3.4",
				},
			},
			want: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:                  "LoadBalancer",
					LoadBalancerIP:        "3.4.5.6",
					ExternalTrafficPolicy: "Local",
					ClusterIP:             "1.2.3.4",
				},
				Status: statusAssigned("3.4.5.6"),
			},
		},

		{
			desc: "No ClusterIP",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type: "LoadBalancer",
				},
			},
			wantErr: false,
		},

		{
			desc: "request IP from wrong ip-family (ipv4)",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:           "LoadBalancer",
					LoadBalancerIP: "1.2.3.1",
					ClusterIP:      "3000::1",
				},
			},
			wantErr: true,
		},

		{
			desc: "request IP from wrong ip-family (ipv6)",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:           "LoadBalancer",
					LoadBalancerIP: "1000::",
					ClusterIP:      "1.2.3.4",
				},
			},
			wantErr: true,
		},

		{
			desc: "IP from wrong ip-family (ipv6) assigned",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "LoadBalancer",
					ClusterIP: "1.2.3.4",
				},
				Status: statusAssigned("1000::"),
			},
			want: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "LoadBalancer",
					ClusterIP: "1.2.3.4",
				},
				Status: statusAssigned("1.2.3.0"),
			},
		},

		{
			desc: "IP from wrong ip-family (ipv4) assigned",
			in: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "LoadBalancer",
					ClusterIP: "3000::1",
				},
				Status: statusAssigned("1.2.3.0"),
			},
			want: &v1.Service{
				Spec: v1.ServiceSpec{
					Type:      "LoadBalancer",
					ClusterIP: "3000::1",
				},
				Status: statusAssigned("1000::"),
			},
		},
	}

	for i := 0; i < 100; i++ {
		for _, test := range tests {
			t.Logf("Running case %q", test.desc)
			k.reset()

			if c.SetBalancer(l, "test", test.in, nil) == k8s.SyncStateError {
				t.Errorf("%q: SetBalancer returned error", test.desc)
				continue
			}
			if test.wantErr != k.loggedWarning {
				t.Errorf("%q: unexpected loggedWarning value, want %v, got %v", test.desc, test.wantErr, k.loggedWarning)
			}

			gotSvc := k.gotService(test.in)

			switch {
			case test.want == nil && gotSvc != nil:
				t.Errorf("%q: unexpectedly mutated service (-in +out)\n%s", test.desc, diffService(test.in, gotSvc))
			case test.want != nil && gotSvc == nil:
				t.Errorf("%q: did not mutate service, wanted (-in +out)\n%s", test.desc, diffService(test.in, test.want))
			case test.want != nil && gotSvc != nil:
				if diff := diffService(test.want, gotSvc); diff != "" {
					t.Errorf("%q: wrong service mutation (-want +got)\n%s", test.desc, diff)
				}
			}

			if test.want != nil && len(test.want.Status.LoadBalancer.Ingress) > 0 && test.want.Status.LoadBalancer.Ingress[0].IP != "" {
				ip := net.ParseIP(test.want.Status.LoadBalancer.Ingress[0].IP)
				if ip == nil {
					panic("bad wanted IP in loadbalancer status")
				}
				if !ip.Equal(c.ips.IP("test")) {
					t.Errorf("%q: controller internal state does not match IP that controller claimed to allocate: want %q, got %q", test.desc, ip, c.ips.IP("test"))
				}
			}
		}

		if t.Failed() {
			// Don't run more test cases if we've already failed, to
			// keep the output readable.
			break
		}

		// Shuffle the input vector, and run again.
		for x := range tests {
			nx := rand.Intn(len(tests) - x)
			tests[x], tests[nx] = tests[nx], tests[x]
		}
		t.Logf("Shuffled test cases")
	}
}
