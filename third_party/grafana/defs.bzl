load("@rules_oci//oci:defs.bzl", "oci_image", "oci_load")
load("@rules_pkg//pkg:tar.bzl", "pkg_tar")

# TODO(tim): Migrate this to a bazel extension
_grafana_plugin_attrs = {
    "version": attr.string(
        mandatory = True,
        doc = "The plugin version",
    ),
    "plugin_name": attr.string(
        mandatory = True,
        doc = "The plugin name",
    ),
}

def _grafana_plugin_impl(ctx):
    ctx.download_and_extract(
        url = "https://grafana.com/api/plugins/%s/versions/%s/download?os=%s&arch=%s" % (ctx.attr.plugin_name, ctx.attr.version, os(ctx), arch(ctx)),
        type = "zip",
    )

    ctx.file("BUILD.bazel", """
package(default_visibility = ["//visibility:public"])

filegroup(
    name = "files",
    srcs = glob(["**"]),
)
    """)

grafana_plugin = repository_rule(
    attrs = _grafana_plugin_attrs,
    implementation = _grafana_plugin_impl,
)

def _grafana_impl(ctx):
    ctx.download_and_extract(
        url = "https://dl.grafana.com/oss/release/grafana-11.4.0.{OS}-{ARCH}.tar.gz".format(
            OS = os(ctx),
            ARCH = arch(ctx),
        ),
        stripPrefix = "grafana-v11.4.0",
    )
    ctx.file("BUILD.bazel", 'exports_files(["bin/grafana-server"])')

grafana = repository_rule(
    implementation = _grafana_impl,
)

def os(ctx):
    os = ctx.os.name.lower()
    if os == "mac os x":
        return "darwin"
    return os

def arch(ctx):
    arch = ctx.os.arch.lower()
    if arch == "aarch64":
        return "arm64"
    return arch

def grafana_plugin_layer(name):
    pkg_tar(
        name = name,
        srcs = ["@{}//:files".format(name)],
        package_dir = "/var/lib/grafana/plugins/{}".format(name),
    )

    return ":{}".format(name)

def grafana_image():
    oci_image(
        name = "grafana_image",
        base = "@grafana",
        tars = [
            grafana_plugin_layer("netsage-sankey-panel"),
            grafana_plugin_layer("grafana-clickhouse-datasource"),
        ],
        visibility = ["//visibility:public"],
    )

    oci_load(
        name = "grafana_load",
        image = ":grafana_image",
        repo_tags = ["github.com/monogon-dev/netmeta/grafana:latest"],
        visibility = ["//visibility:public"],
    )
