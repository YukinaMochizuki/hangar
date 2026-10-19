package cli

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	iamv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/iam/v1alpha1"
	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/client"
)

func (a *app) adminCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Run the platform: invite people (platform.root)"}
	cmd.AddCommand(a.inviteCmd(), a.invitationsCmd(), a.revokeCmd())
	return cmd
}

var notLabelChars = regexp.MustCompile(`[^a-z0-9]+`)

// inviteBindingName is the name of the platform RoleBinding hangar admin
// invite makes for email: readable, and the same every time, so inviting
// again replaces it and revoking finds it.
func inviteBindingName(email string) string {
	sum := sha256.Sum256([]byte(email))
	tail := hex.EncodeToString(sum[:])[:8]
	slug := strings.Trim(notLabelChars.ReplaceAllString(email, "-"), "-")
	slug = strings.TrimRight(slug[:min(len(slug), 40)], "-")
	if slug == "" {
		return "invite-" + tail
	}
	return "invite-" + slug + "-" + tail
}

// emailArg reads an EMAIL argument as bindings hold it, in lower case. One
// that is not ASCII is refused before lower-casing, which could fold it
// into someone else's address (U+212A KELVIN SIGN becomes k).
func emailArg(s string) (string, error) {
	s = strings.TrimSpace(s)
	if !auth.ASCII(s) {
		return "", usageErr("EMAIL %q must be ASCII", s)
	}
	return strings.ToLower(s), nil
}

// users maps the email of every User to its username.
func users(cmd *cobra.Command, cl *client.Client) (map[string]string, error) {
	objs, _, err := cl.ListAll(cmd.Context(), registry.User, client.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, o := range objs {
		u := o.(*iamv1alpha1.User)
		out[u.GetSpec().GetEmail()] = u.GetMetadata().GetName()
	}
	return out, nil
}

// platformBindings returns every platform-scope RoleBinding naming a person,
// by their email, in name order: however they were made, by hangar admin
// invite or by hand.
func platformBindings(cmd *cobra.Command, cl *client.Client) (map[string][]*iamv1alpha1.RoleBinding, error) {
	objs, _, err := cl.ListAll(cmd.Context(), registry.RoleBinding, client.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := map[string][]*iamv1alpha1.RoleBinding{}
	for _, o := range objs {
		rb := o.(*iamv1alpha1.RoleBinding)
		if email := rb.GetSpec().GetSubject().GetUser(); email != "" && rb.GetSpec().GetScope().GetPlatform() != nil {
			out[email] = append(out[email], rb)
		}
	}
	return out, nil
}

func (a *app) inviteCmd() *cobra.Command {
	var plan, role string
	var maxTenants int32
	cmd := &cobra.Command{
		Use:   "invite EMAIL [--plan PLAN] [--max-tenants N] [--role ROLE]",
		Short: "Invite a person: bind them a platform role and record their plan and tenant cap",
		Long: "Makes the Invitation of EMAIL, which their User starts with when they sign up:\n" +
			"the Plan their tenants get (the default plan without --plan) and how many\n" +
			"tenants they may own (the platform default without --max-tenants, and at\n" +
			"least 1, the tenant signing up opens). Then binds them the platform role,\n" +
			"which lets them sign in and sign up, unless a platform binding already\n" +
			"gives them that role; one giving another is left alone and refused, as\n" +
			"changing someone's platform role is not inviting them. Inviting again\n" +
			"replaces the Invitation.\n" +
			"\n" +
			"Someone who has signed up before, and was removed, gets only the role\n" +
			"back; their User keeps its plan and cap, which apply changes.",
		Example: "  hangar admin invite alice@example.com\n" +
			"  hangar admin invite bob@example.com --plan sponsor --max-tenants 3",
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			email, err := emailArg(args[0])
			if err != nil {
				return err
			}
			if !strings.HasPrefix(role, "platform.") {
				return usageErr("--role %q: an invitation binds a platform role, such as platform.member", role)
			}
			capped := cmd.Flags().Changed("max-tenants")
			if capped && maxTenants < 1 {
				return usageErr("--max-tenants %d: at least 1, as signing up opens a tenant", maxTenants)
			}
			cl, _, err := a.client()
			if err != nil {
				return err
			}
			signedUp, err := users(cmd, cl)
			if err != nil {
				return err
			}
			user := signedUp[email]
			if user != "" && (plan != "" || capped) {
				return fmt.Errorf("%s has signed up before, as %s; set spec.plan or spec.maxOwnedTenants on user %s with apply", email, user, user)
			}
			bindings, err := platformBindings(cmd, cl)
			if err != nil {
				return err
			}
			var bound *iamv1alpha1.RoleBinding
			for _, rb := range bindings[email] {
				if rb.GetSpec().GetRole() == role {
					bound = rb
				}
			}
			if bound == nil && len(bindings[email]) > 0 {
				rb := bindings[email][0]
				return fmt.Errorf("%s already holds %s through rolebinding %s; change or delete that binding to give them %s",
					email, rb.GetSpec().GetRole(), rb.GetMetadata().GetName(), role)
			}
			w := cmd.OutOrStdout()
			if user == "" {
				spec := &iamv1alpha1.InvitationSpec{Plan: plan}
				if capped {
					spec.MaxOwnedTenants = proto.Int32(maxTenants)
				}
				// The Invitation first, so it is there by the time the role
				// lets them sign up.
				if _, err := cl.Apply(cmd.Context(), registry.Invitation, &iamv1alpha1.Invitation{
					Metadata: &metav1.ObjectMeta{Name: email}, Spec: spec,
				}); err != nil {
					return err
				}
			}
			if bound != nil {
				_, _ = fmt.Fprintf(w, "%s already holds %s through rolebinding %s\n", email, role, bound.GetMetadata().GetName())
			} else if _, err := cl.Apply(cmd.Context(), registry.RoleBinding, &iamv1alpha1.RoleBinding{
				Metadata: &metav1.ObjectMeta{Name: inviteBindingName(email)},
				Spec: &iamv1alpha1.RoleBindingSpec{
					Subject: &iamv1alpha1.Subject{Subject: &iamv1alpha1.Subject_User{User: email}},
					Role:    role,
					Scope:   &iamv1alpha1.BindingScope{Target: &iamv1alpha1.BindingScope_Platform{}},
				},
			}); err != nil {
				if user == "" {
					return fmt.Errorf("the invitation of %s is made, but binding %s failed; run this again: %w", email, role, err)
				}
				return err
			}
			if user != "" {
				if bound == nil {
					_, _ = fmt.Fprintf(w, "%s signed up before, as %s: bound %s again\n", email, user, role)
				}
				return nil
			}
			tenants := "the platform default"
			if capped {
				tenants = strconv.Itoa(int(maxTenants))
			}
			_, _ = fmt.Fprintf(w, "invited %s as %s, plan %s, tenants %s; they sign in, then run hangar signup --username NAME\n",
				email, role, cmp.Or(plan, "default"), tenants)
			return nil
		},
	}
	cmd.Flags().StringVar(&plan, "plan", "", "the Plan their tenants get; empty is the default plan")
	cmd.Flags().Int32Var(&maxTenants, "max-tenants", 0, "how many tenants they may own; unset is the platform default")
	cmd.Flags().StringVar(&role, "role", "platform.member", "the platform role to bind them")
	return cmd
}

func (a *app) invitationsCmd() *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "invitations",
		Short: "List the people invited who have not signed up yet",
		Long: "Lists everyone invited who has not signed up: each Invitation, and each\n" +
			"email a platform role binding names that has no User, such as one bound\n" +
			"by hand, which starts with the defaults (NOTE says so). -o json, yaml\n" +
			"and name print the Invitation objects alone.",
		Args: args(0, 0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			format := cmp.Or(output, "table")
			if err := checkOutput(format); err != nil {
				return err
			}
			cl, _, err := a.client()
			if err != nil {
				return err
			}
			objs, _, err := cl.ListAll(cmd.Context(), registry.Invitation, client.ListOptions{})
			if err != nil {
				return err
			}
			if format != "table" {
				return a.printObjects(cmd.OutOrStdout(), format, registry.Invitation, objs, false, false)
			}
			signedUp, err := users(cmd, cl)
			if err != nil {
				return err
			}
			bindings, err := platformBindings(cmd, cl)
			if err != nil {
				return err
			}
			return a.printInvitations(cmd.OutOrStdout(), objs, bindings, signedUp)
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "", "table, json, yaml or name")
	return cmd
}

// printInvitations is the table of people invited who have not signed up:
// whom, with which role, plan and cap, by email.
func (a *app) printInvitations(w io.Writer, invitations []proto.Message, bindings map[string][]*iamv1alpha1.RoleBinding, signedUp map[string]string) error {
	type row struct {
		inv *iamv1alpha1.Invitation
		rbs []*iamv1alpha1.RoleBinding
	}
	rows := map[string]*row{}
	for _, o := range invitations {
		inv := o.(*iamv1alpha1.Invitation)
		rows[inv.GetMetadata().GetName()] = &row{inv: inv}
	}
	for email, rbs := range bindings {
		if signedUp[email] != "" {
			continue
		}
		if rows[email] == nil {
			rows[email] = &row{}
		}
		rows[email].rbs = rbs
	}
	emails := slices.Sorted(maps.Keys(rows))
	now := a.opts.Now()
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "EMAIL\tROLE\tPLAN\tMAX TENANTS\tAGE\tNOTE")
	for _, email := range emails {
		r := rows[email]
		role, plan, tenants, created, note := "-", "default", "default", "-", ""
		var roles []string
		for _, rb := range r.rbs {
			roles = append(roles, rb.GetSpec().GetRole())
		}
		if len(roles) > 0 {
			role = strings.Join(roles, ",")
			created = age(now, r.rbs[0].GetMetadata())
		}
		switch {
		case r.inv == nil:
			note = "no invitation: starts with the defaults"
		case len(r.rbs) == 0:
			note = "no platform role: cannot sign up"
		}
		if r.inv != nil {
			plan = cmp.Or(r.inv.GetSpec().GetPlan(), "default")
			if r.inv.GetSpec().MaxOwnedTenants != nil {
				tenants = strconv.Itoa(int(r.inv.GetSpec().GetMaxOwnedTenants()))
			}
			created = age(now, r.inv.GetMetadata())
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", email, role, plan, tenants, created, note)
	}
	return tw.Flush()
}

func (a *app) revokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke EMAIL",
		Short: "Revoke an invitation no one has taken up yet",
		Long: "Deletes every platform role binding naming EMAIL, however it was made,\n" +
			"and the Invitation of EMAIL, so the person can no longer sign up, and\n" +
			"prints what it deleted. An invitation taken up is not revoked: that\n" +
			"person is a member, whose platform role binding is what to delete.",
		Args: args(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			email, err := emailArg(args[0])
			if err != nil {
				return err
			}
			cl, _, err := a.client()
			if err != nil {
				return err
			}
			signedUp, err := users(cmd, cl)
			if err != nil {
				return err
			}
			if user := signedUp[email]; user != "" {
				return fmt.Errorf("%s has signed up, as %s, so there is no invitation to revoke; removing a member is deleting their platform role binding", email, user)
			}
			bindings, err := platformBindings(cmd, cl)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			deleted := 0
			del := func(r registry.Resource, name, what string) error {
				_, err := cl.Delete(cmd.Context(), r, client.Key{Name: name})
				switch {
				case err == nil:
					deleted++
					_, _ = fmt.Fprintf(w, "deleted %s %s%s\n", strings.ToLower(r.Kind), name, what)
				case connect.CodeOf(err) != connect.CodeNotFound:
					return err
				}
				return nil
			}
			// The bindings first: without them no one signs up, whatever
			// happens to the Invitation.
			for _, rb := range bindings[email] {
				if err := del(registry.RoleBinding, rb.GetMetadata().GetName(), " ("+rb.GetSpec().GetRole()+")"); err != nil {
					return err
				}
			}
			if err := del(registry.Invitation, email, ""); err != nil {
				return err
			}
			if deleted == 0 {
				return &exitError{code: ExitNotFound, err: fmt.Errorf("no invitation of %s, and no platform role binding naming it", email)}
			}
			_, _ = fmt.Fprintf(w, "revoked the invitation of %s\n", email)
			return nil
		},
	}
}
