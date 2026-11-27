package k8s

command: diff: {
	task: kube: {
		kind:  "exec"
		cmd:   "k3s kubectl diff --server-side --force-conflicts -f -"
		stdin: all_objects_yaml
	}
}
