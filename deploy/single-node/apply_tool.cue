package k8s

import "encoding/yaml"

command: apply: {
	task: apply: {
		$after: task.prereqs
		kind:   "exec"
		cmd:    "k3s kubectl apply --server-side --force-conflicts --all -f -"
		stdin:  all_objects_yaml
	}
}

command: "apply-prune": {
	task: kube: {
		kind:  "exec"
		cmd:   "k3s kubectl apply --server-side --force-conflicts --all -f - --prune=true"
		stdin: all_objects_yaml
	}
}

// Applies configmaps only
command: "apply-cm": {
	task: apply: {
		kind:   "exec"
		cmd:    "k3s kubectl apply --server-side --force-conflicts --all -f -"
		stdin:  yaml.MarshalStream([ for v in all_objects if v.kind == "ConfigMap" {v}])
		stdout: string
	}
	task: applyDisplay: {
		kind: "print"
		text: task.apply.stdout
	}
}
