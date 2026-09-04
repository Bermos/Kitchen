# The image behind the several-workloads end-to-end case.
#
# One file, two shippable stages, and that is the point of it. The stages are
# identical but for the word each of them bakes into `STAGE`, so an image can
# be asked which stage it came out of and answer — which is the only way a
# test can tell whether `dockerfileTarget` was honoured, since the assertion is
# about the artifact that was pushed rather than about anything the reconciler
# wrote down.
#
# `final` is written last, so it is what a build that names no target ships;
# `shipped` is what the unit's `api` workload names in kitchen.json. One
# commit, one Dockerfile, two images, and the only difference between them is
# the target.
#
# busybox because a kind node has two minutes' patience: the whole image is one
# base layer everything else in this fixture already needs, `httpd` is in it,
# and it is pulled through Google's Docker Hub mirror so an anonymous rate
# limit cannot fail a build that has nothing to do with rate limits.
FROM mirror.gcr.io/library/busybox:1.37 AS base
COPY serve.sh /serve.sh
COPY migrate.sh /migrate.sh
RUN mkdir -p /www && chmod 0755 /serve.sh /migrate.sh

# The stage `dockerfileTarget` names. Not the last one, deliberately: a target
# that selected the file's last stage would be indistinguishable from no
# target at all.
FROM base AS shipped
ENV STAGE=shipped
CMD ["/serve.sh"]

# The file's last stage, and so what BuildKit ships when nothing says
# otherwise. The web process is built with no target and must come out of
# here.
FROM base AS final
ENV STAGE=final
CMD ["/serve.sh"]
