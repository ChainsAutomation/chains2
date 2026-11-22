package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	chains "chains-automation"
)

func printDescriptions(out io.Writer, format, selector string, services []chains.Service) error {
	services = slices.Clone(services)
	slices.SortFunc(services, func(a, b chains.Service) int {
		if n := strings.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		return strings.Compare(a.UUID, b.UUID)
	})
	if selector != "" {
		if index := slices.IndexFunc(services, func(s chains.Service) bool { return s.UUID == selector }); index >= 0 {
			services = services[index : index+1]
		} else {
			services = slices.DeleteFunc(services, func(s chains.Service) bool { return s.Name != selector })
		}
		if len(services) == 0 {
			return fmt.Errorf("no registered service matches %q", selector)
		}
	}
	if format == "json" {
		return json.NewEncoder(out).Encode(services)
	}
	if selector == "" && format == "table" {
		rows := make([][]string, 0, len(services))
		for _, s := range services {
			attributes := 0
			for _, d := range s.Devices {
				attributes += len(d.Attributes)
			}
			rows = append(rows, []string{s.Name, s.UUID, s.ServiceType, strconv.Itoa(len(s.Devices)), strconv.Itoa(attributes), strconv.Itoa(len(s.Events)), strconv.Itoa(len(s.Actions))})
		}
		return printRows(out, format, []string{"SERVICE", "ID", "TYPE", "DEVICES", "ATTRIBUTES", "EVENTS", "ACTIONS"}, rows)
	}

	var rows [][]string
	for _, s := range services {
		rows = append(rows, []string{"service", s.UUID, "", s.Name, "", s.ServiceType, s.Description, ""})
		devices := slices.Clone(s.Devices)
		slices.SortFunc(devices, func(a, b chains.Device) int { return strings.Compare(a.ID, b.ID) })
		for _, d := range devices {
			details := descriptionFields(d.Description,
				describeList("Classes", d.Classes, ", "), describeList("Location", d.Location, " › "),
				describeList("Tags", d.Tags, ", "), describeList("Actions", d.Actions, ", "))
			rows = append(rows, []string{"device", s.UUID, d.ID, d.Name, "", "", details, ""})
			for _, name := range slices.Sorted(maps.Keys(d.Attributes)) {
				a := d.Attributes[name]
				access := "read-only"
				if a.Setter != nil {
					access = "setter " + a.Setter.Action + "@" + a.Setter.Version + " (" + a.Setter.Argument + ")"
				}
				rows = append(rows, []string{"attribute", s.UUID, d.ID, name, "", access,
					descriptionFields(a.Label, a.Description, describeList("Unit", []string{a.Unit}, "")), string(a.Schema)})
			}
		}
		for _, kind := range []struct {
			name  string
			items map[string]map[string]json.RawMessage
		}{{"event", s.Events}, {"action", s.Actions}} {
			for _, name := range slices.Sorted(maps.Keys(kind.items)) {
				for _, version := range slices.Sorted(maps.Keys(kind.items[name])) {
					var detail string
					if kind.name == "action" && slices.Contains(s.IdempotentActions, name) {
						detail = "idempotent"
					}
					rows = append(rows, []string{kind.name, s.UUID, "", name, version, detail, "", string(kind.items[name][version])})
				}
			}
		}
	}
	headers := []string{"kind", "service_id", "device_id", "name", "version", "capability", "details", "schema"}
	if format == "table" {
		for i, header := range headers {
			headers[i] = strings.ToUpper(header)
		}
	}
	return printRows(out, format, headers, rows)
}

func describeList(label string, values []string, separator string) string {
	text := strings.Join(values, separator)
	if text == "" {
		return ""
	}
	return label + ": " + text
}

func descriptionFields(values ...string) string {
	return strings.Join(slices.DeleteFunc(values, func(s string) bool { return s == "" }), " · ")
}
