# A page with no build step at all, which is the case Buildpacks cannot take.
# Detection fails in about nine seconds with "No buildpack groups passed
# detection", so a static site reaches the platform through this path or not at
# all. Proved on the cluster 2026-08-14.
#
# nginx-unprivileged rather than nginx: the stock image runs as root and the
# platform refuses it with image_runs_as_root before composing anything. This
# one already runs as a non root user and already listens on 8080, which is
# what the platform hands the container as PORT.
FROM nginxinc/nginx-unprivileged:1.29-alpine

COPY index.html /usr/share/nginx/html/index.html
