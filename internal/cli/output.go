package cli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"sigs.k8s.io/yaml"

	metav1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/meta/v1"
	networkv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/network/v1alpha1"
	"github.com/YukinaMochizuki/hangar/gen/go/hangar/registry"
	secretsv1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/secrets/v1alpha1"
	storagev1alpha1 "github.com/YukinaMochizuki/hangar/gen/go/hangar/storage/v1alpha1"
	"github.com/YukinaMochizuki/hangar/internal/apimachinery/object"
)

var outputFormats = []string{"table", "json", "yaml", "name"}

func checkOutput(format string) error {
	for _, f := range outputFormats {
		if format == f {
			return nil
		}
	}
	return usageErr("unknown output %q; want one of %s", format, strings.Join(outputFormats, ", "))
}

func marshalJSON(m proto.Message) (json.RawMessage, error) {
	return protojson.Marshal(m)
}

// printObjects writes objects in the format. A single object (from a get by
// name) prints bare; otherwise JSON and YAML wrap them in {"items": [...]} so
// the output is always one document.
func (a *app) printObjects(w io.Writer, format string, r registry.Resource, objs []proto.Message, single, allProjects bool) error {
	switch format {
	case "json", "yaml":
		var doc any
		if single && len(objs) == 1 {
			js, err := marshalJSON(objs[0])
			if err != nil {
				return err
			}
			doc = js
		} else {
			items := make([]json.RawMessage, 0, len(objs))
			for _, o := range objs {
				js, err := marshalJSON(o)
				if err != nil {
					return err
				}
				items = append(items, js)
			}
			doc = map[string]any{"items": items}
		}
		js, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if format == "yaml" {
			y, err := yaml.JSONToYAML(js)
			if err != nil {
				return err
			}
			_, err = w.Write(y)
			return err
		}
		_, err = fmt.Fprintln(w, string(js))
		return err
	case "name":
		for _, o := range objs {
			if _, err := fmt.Fprintln(w, objectName(r, o)); err != nil {
				return err
			}
		}
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	header := "NAME\tREADY\tAGE"
	switch r.Kind {
	case registry.Secret.Kind:
		header += "\tREGISTRIES"
	case registry.Disk.Kind:
		header += "\tUSED AS"
	case registry.PortForward.Kind:
		header += "\tIPV4\tPORT\tPROTOCOL\tTARGET"
	}
	if allProjects && r.ProjectScoped() {
		header = "PROJECT\t" + header
	}
	_, _ = fmt.Fprintln(tw, header)
	for _, o := range objs {
		meta := object.Meta(o)
		row := fmt.Sprintf("%s\t%s\t%s", meta.GetName(), readyColumn(o), age(a.opts.Now(), meta))
		switch o := o.(type) {
		case *secretsv1alpha1.Secret:
			row += "\t" + cmp.Or(strings.Join(o.GetStatus().GetRegistries(), ","), "-")
		case *storagev1alpha1.Disk:
			row += "\t" + usedAs(o.GetStatus().GetAttachment())
		case *networkv1alpha1.PortForward:
			row += "\t" + forwardColumns(o)
		}
		if allProjects && r.ProjectScoped() {
			row = meta.GetProject() + "\t" + row
		}
		_, _ = fmt.Fprintln(tw, row)
	}
	return tw.Flush()
}

// usedAs says what a disk is attached to and as what, such as "volume of
// container/db"; "-" when it is attached to none.
func usedAs(a *storagev1alpha1.DiskAttachment) string {
	if a == nil {
		return "-"
	}
	role := strings.ToLower(strings.TrimPrefix(a.GetAs().String(), "DISK_ATTACHMENT_ROLE_"))
	return role + " of " + strings.ToLower(cmp.Or(a.GetKind(), "machine")) + "/" + cmp.Or(a.GetName(), a.GetMachine())
}

// forwardColumns are a port forward's columns: its address, public port,
// protocol, and the interface and port it leads to, such as
// "container/web:eth0:8080"; "-" for what it does not have yet.
func forwardColumns(pf *networkv1alpha1.PortForward) string {
	port := "-"
	if p := pf.GetStatus().GetPort(); p != 0 {
		port = strconv.Itoa(int(p))
	}
	protocol := strings.TrimPrefix(pf.GetSpec().GetProtocol().String(), "PROTOCOL_")
	t := pf.GetSpec().GetTarget()
	target := "-"
	switch {
	case t.GetVirtualMachine() != "":
		target = "virtualmachine/" + t.GetVirtualMachine()
	case t.GetContainer() != "":
		target = "container/" + t.GetContainer()
	}
	if target != "-" {
		target += ":" + cmp.Or(t.GetInterface(), "eth0") + ":" + strconv.Itoa(int(pf.GetSpec().GetTargetPort()))
	}
	return strings.Join([]string{cmp.Or(pf.GetStatus().GetIpv4(), "-"), port, protocol, target}, "\t")
}

func objectName(r registry.Resource, o proto.Message) string {
	return strings.ToLower(r.Kind) + "/" + object.Meta(o).GetName()
}

func readyColumn(o proto.Message) string {
	c := condition(o, "Ready")
	if c == nil {
		return "-"
	}
	switch c.GetStatus() {
	case metav1.ConditionStatus_CONDITION_STATUS_TRUE:
		return "True"
	case metav1.ConditionStatus_CONDITION_STATUS_FALSE:
		return "False"
	}
	return "Unknown"
}

func age(now time.Time, meta *metav1.ObjectMeta) string {
	if meta.GetCreateTime() == nil {
		return "-"
	}
	d := now.Sub(meta.GetCreateTime().AsTime())
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// condition returns the status condition of the given type, if any.
func condition(o proto.Message, typ string) *metav1.Condition {
	st := object.Status(o).ProtoReflect()
	fd := st.Descriptor().Fields().ByName("conditions")
	if fd == nil {
		return nil
	}
	list := st.Get(fd).List()
	for i := range list.Len() {
		c, ok := list.Get(i).Message().Interface().(*metav1.Condition)
		if ok && c.GetType() == typ {
			return c
		}
	}
	return nil
}
