def _cue_export(ctx):
    arguments = ctx.actions.args()
    arguments.add("export")

    if ctx.attr.out != "":
        arguments.add_all(["--out", ctx.attr.out])

    if ctx.attr.expression != "":
        arguments.add_all(["--expression", ctx.attr.expression])

    if len(ctx.attr.args) != 0:
        for arg in ctx.attr.args:
            arguments.add(ctx.expand_location(arg))

    output = ctx.actions.declare_file(ctx.attr.outfile)
    arguments.add_all(["--outfile", output])

    ctx.actions.run(
        outputs = [output],
        inputs = ctx.files.srcs,
        arguments = [arguments],
        executable = ctx.executable._cue,
        env = {
            "CUE_CACHE_DIR": "$TMPDIR",
            "CUE_EXPERIMENT": "embed",
        },
    )

    return DefaultInfo(
        files = depset([output]),
    )

cue_export = rule(
    implementation = _cue_export,
    attrs = {
        "out": attr.string(),
        "expression": attr.string(),
        "args": attr.string_list(),
        "srcs": attr.label_list(
        ),
        "outfile": attr.string(
            mandatory = True,
        ),
        "_cue": attr.label(
            default = "@org_cuelang_go//cmd/cue",
            cfg = "exec",
            executable = True,
        ),
    },
)
