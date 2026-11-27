def _cue_local_images(ctx):
    inputs = []
    arguments = ctx.actions.args()
    for image, target in ctx.attr.images.items():
        manifest = None
        for file in target.data_runfiles.files.to_list():
            if file.basename == "manifest.json":
                manifest = file
                break
        if manifest == None:
            fail("Can't find manifest for image: {}" % image)

        inputs.append(manifest)
        inputs += target.files.to_list()

        arguments.add_all(["-image", image, "-manifest", manifest])

    output = ctx.actions.declare_file("images.cue")
    arguments.add(output)

    ctx.actions.run(
        outputs = [output],
        inputs = inputs,
        arguments = [arguments],
        executable = ctx.executable._generate_images,
    )

    return DefaultInfo(
        files = depset([output]),
    )

cue_local_images = rule(
    implementation = _cue_local_images,
    attrs = {
        "images": attr.string_keyed_label_dict(),
        "_generate_images": attr.label(
            default = "//build/cue/local-images",
            cfg = "exec",
            executable = True,
        ),
    },
)
