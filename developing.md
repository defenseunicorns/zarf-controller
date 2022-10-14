#  Developing

The controller pulls artifacts from the source controller deployed by flux.  When developing locally, the sevice isnt available to our development machines, and port-forwarding was being flakey, so I ended up getting a VS to expose the source countroller and then I just set the environment variable 

```bash
export SOURCE_CONTROLLER_LOCALHOST=source-controller.bigbang.dev
make run
```