package apiserver

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"strings"

	"connectrpc.com/connect"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	tenancyv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/tenancy/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/resource"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

const (
	// nameAlphabet is what the suffix of the name signing up gives a
	// person's first tenant and project is drawn from: lowercase letters and
	// digits, but 0, o, 1, l and i, which are read for one another.
	nameAlphabet  = "abcdefghjkmnpqrstuvwxyz23456789"
	nameSuffixLen = 5
	// maxNameBase bounds the part of that name taken from the username, so
	// with its suffix the name is at most 26 characters and, like the names
	// hangar create suggests, leaves room in a hostname label for what is
	// put before it.
	maxNameBase = 20
	// nameAttempts bounds how many names are tried before signing up gives
	// up; with 31^5 suffixes for each username, a second is rarely needed.
	nameAttempts = 10
	// DefaultProjectDisplayName is the display name of the project signing
	// up makes.
	DefaultProjectDisplayName = "Default"
)

// randomNameSuffix returns nameSuffixLen characters of nameAlphabet.
func randomNameSuffix() string {
	var b strings.Builder
	for range nameSuffixLen {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(nameAlphabet))))
		if err != nil {
			panic(err) // crypto/rand does not fail on supported platforms.
		}
		b.WriteByte(nameAlphabet[n.Int64()])
	}
	return b.String()
}

// onboardingName is the name of a person's first tenant and project:
// their username, cut to maxNameBase characters and with no '-' at its end,
// then '-' and suffix. A username is a DNS label, so the name is one too.
func onboardingName(username, suffix string) string {
	base := strings.TrimRight(username[:min(len(username), maxNameBase)], "-")
	return base + "-" + suffix
}

// freeOnboardingName finds a name no tenant and no project has ever had.
// It runs in the transaction that creates them, which every other write
// waits for, so the name is still free when they are created.
func (s *Server) freeOnboardingName(ctx context.Context, username string) (string, error) {
	for range nameAttempts {
		name := onboardingName(username, s.nameSuffix())
		free := true
		for _, r := range []registry.Resource{registry.Tenant, registry.Project} {
			_, err := s.store.Get(ctx, nameClaim(r, resource.Key{Name: name}))
			if err == nil {
				free = false
				break
			}
			if !errors.Is(err, store.ErrNotFound) {
				return "", err
			}
		}
		if free {
			return name, nil
		}
	}
	return "", connect.NewError(connect.CodeAborted, errors.New("found no free name for your first tenant; sign up again"))
}

// onboard signs p up as username, in one store transaction: their User, from
// their Invitation, which it deletes, and a tenant they own with a project and
// its default network (docs/decisions/tenancy-and-iam.md).
func (s *Server) onboard(ctx context.Context, p auth.Principal, username, displayName string) (*iamv1alpha1.SignUpResponse, error) {
	s.users.Lock()
	defer s.users.Unlock()
	s.plans.Lock()
	defer s.plans.Unlock()
	// The default network's writes hold attach, which every write waiting
	// for this transaction may hold: it is taken before the transaction.
	s.attach.Lock()
	defer s.attach.Unlock()
	ctx = holding(s.system(ctx), &s.users, &s.plans, &s.attach)
	res := &iamv1alpha1.SignUpResponse{}
	err := s.atomically(ctx, func(ctx context.Context) error {
		inv, err := s.invitationOf(ctx, p.ID)
		if err != nil {
			return err
		}
		spec := &iamv1alpha1.UserSpec{Email: p.ID, DisplayName: displayName}
		if inv != nil && inv.GetSpec() != nil {
			spec.Plan, spec.MaxOwnedTenants = inv.GetSpec().GetPlan(), inv.GetSpec().MaxOwnedTenants
		}
		u, err := s.backend(registry.User).Create(ctx, &iamv1alpha1.User{
			Metadata: &metav1.ObjectMeta{Name: username}, Spec: spec,
		})
		if connect.CodeOf(err) == connect.CodeAlreadyExists {
			return failedPrecondition("username %q is taken; choose another", username)
		}
		if err != nil {
			return err
		}
		res.User = u.Object.(*iamv1alpha1.User)

		name, err := s.freeOnboardingName(ctx, username)
		if err != nil {
			return err
		}
		// Opened by the person, as if they had created it themselves: it
		// counts toward their cap.
		t, err := s.backend(registry.Tenant).Create(ctx, &tenancyv1alpha1.Tenant{
			Metadata: &metav1.ObjectMeta{Name: name, Annotations: map[string]string{OpenedBy: p.String()}},
			Spec:     &tenancyv1alpha1.TenantSpec{DisplayName: username, Plan: res.User.GetSpec().GetPlan()},
		})
		if err != nil {
			return err
		}
		if err := grantTenantOwner(ctx, s, p, t.Object); err != nil {
			return err
		}
		if _, err := s.backend(registry.Project).Create(ctx, &tenancyv1alpha1.Project{
			Metadata: &metav1.ObjectMeta{Name: name},
			Spec:     &tenancyv1alpha1.ProjectSpec{Tenant: name, DisplayName: DefaultProjectDisplayName},
		}); err != nil {
			return err
		}
		// The project controller would make it too, but later: made here,
		// the project is ready as soon as signing up answers.
		if _, err := s.backend(registry.Network).Create(ctx, &networkv1alpha1.Network{
			Metadata: &metav1.ObjectMeta{Project: name, Name: DefaultNetwork},
			Spec:     &networkv1alpha1.NetworkSpec{Ipv4Cidr: DefaultNetworkCIDR},
		}); err != nil {
			return err
		}
		if inv != nil {
			if _, err := s.backend(registry.Invitation).Delete(ctx, resource.Key{Name: p.ID}, nil); err != nil {
				return err
			}
		}
		res.Tenant, res.Project = name, name
		return nil
	})
	if err != nil {
		return nil, apiError(err)
	}
	return res, nil
}
