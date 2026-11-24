# RedHat-family fleet node.
#
# Carries two independent Puppet implementations so voxdiff can compare them:
#   /opt/puppetlabs/bin/puppet   OpenVox agent 8.28.1, openfact 5.6.1  (RPM)
#   /usr/local/bin/puppet        puppet gem 8.10.0,    facter 4.10.0   (system ruby)
#
# The fact engines differ too, and they disagree on real values: openfact reports
# processors.isa as "x86_64" where facter 4 reports "unknown". Check 2 therefore
# varies implementation, version AND fact engine together, and a divergence it
# reports must not be attributed to the implementation alone.
#
# Separation, precisely: the two code trees do not collide, because the RPM keeps
# everything under /opt/puppetlabs with its own vendored ruby while the gem
# installs against the distro ruby, and voxdiff is always given an explicit
# binary path so PATH order never decides which runs. Their *settings* do
# collide -- both resolve the same confdir, vardir, libdir and ssldir, so a
# plugin synced by one is loaded by the other. Runtime isolation comes from the
# gate scripts passing -confdir and -vardir into throwaway directories, not from
# this image.
FROM quay.io/centos/centos:stream10

# rubygem-racc is load-bearing, not a prunable build dep: Puppet's parser is
# racc-generated and CentOS Stream 10's ruby RPM does not bundle it, so without
# it the gem Puppet dies with "cannot load such file -- racc/parser.rb".
RUN dnf -y install https://yum.voxpupuli.org/openvox8-release-el-10.noarch.rpm && \
    dnf -y install openvox-agent openvoxdb-termini \
                   ruby ruby-devel rubygem-racc gcc make redhat-rpm-config \
                   curl openssl ca-certificates && \
    dnf clean all

# facter is pinned explicitly rather than left to dependency resolution.
# `gem install puppet -v 8.10.0` pins Puppet and nothing beneath it, and facter
# is the dependency that decides check 2's headline measurement -- leaving it
# to float means two builds a month apart can report different verdicts from
# identical source. The other transitive gems do not affect fact values and are
# left to float -- tracked debt in its own right, alongside the base image and
# the unversioned openvox-agent install above. The vendored module repos used to
# share that debt and no longer do: all six are commit-pinned in vendor.sh and
# verified against on every run.
RUN gem install --no-document facter -v 4.10.0 && \
    gem install --no-document puppet -v 8.10.0

ENV PATH="/opt/puppetlabs/bin:${PATH}"

CMD ["tail", "-f", "/dev/null"]
