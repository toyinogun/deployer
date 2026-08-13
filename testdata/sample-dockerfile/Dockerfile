# The app the Dockerfile build path deploys (spec 0009, AC-17). Deliberately the
# smallest thing that proves the path is real: a base image pull, one RUN layer
# that BuildKit has to execute, a non root USER so the image is not refused with
# image_runs_as_root, and a server on the port the platform gives it.
FROM busybox:1.37

RUN mkdir -p /www \
 && echo 'hello from the dockerfile path' > /www/index.html

# Not root, which is the platform's rule for any image it will run. Removing this
# line is how AC-12's refusal is proved.
USER 1000

# PORT is what the platform sets; the default is only for running this by hand.
ENTRYPOINT ["sh", "-c", "exec httpd -f -v -p \"${PORT:-8080}\" -h /www"]
