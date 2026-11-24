# Exercises the constructs the fleet exists to test: hiera-driven parameters
# including a deep-merged hash, a simplib function call, fact-driven branching,
# and (on the exporter) an exported resource.
#
# Every parameter type is lower-bounded, and fleet_tier is fatal when absent,
# because every gate in this harness compares a node against itself. Nothing
# cross-checks one node against another, so data that silently degrades --
# an empty string, an empty hash, an unmounted facts.d -- would produce a
# uniform fleet that still passes every check. Degenerate input has to fail at
# compile time or it will not be noticed at all.
class fleet_profile (
  String[1]                               $message,
  Hash[String[1], Array[String[1], 1], 1] $packages,
  Boolean                                 $export_resources = false,
  Boolean                                 $collect_resources = false,
  String[1]                               $compliance_marker = 'unenforced',
) {
  # simplib: a real SIMP function against real facts, called once each way.
  # host_is_me builds its identifier list starting with the networking.fqdn
  # fact, so passing that fact straight back can only return true -- on its
  # own it is a tautology that a stubbed implementation would satisfy. The
  # TEST-NET-1 address can only return false, so the pair carries information
  # the single call did not.
  $is_me     = simplib::host_is_me($facts['networking']['fqdn'])
  $is_not_me = simplib::host_is_me('192.0.2.1')

  notify { 'fleet-message':
    message => "${message}/${is_me}/${is_not_me}",
  }

  # Deep-merged hash from hiera, iterated in sorted key order so the manifest
  # itself contributes no ordering ambiguity to the catalog. Note this also
  # means hiera's own merge ordering is not observable here: the merge earns
  # its place by exercising multi-layer merge correctness and by exposing
  # membership and value changes, not ordering ones.
  $packages.keys.sort.each |String $group| {
    $packages[$group].sort.each |String $pkg| {
      notify { "fleet-pkg-${group}-${pkg}":
        message => "${group}:${pkg}",
      }
    }
  }

  # Fact-driven branching: distinguishes node-el from node-el-alt, which share
  # an image, an OS family and every hiera layer but one, and differ only in
  # this external fact. A missing or misdirected facts.d mount is therefore
  # fatal rather than defaulted -- defaulting it would collapse those two
  # nodes into the same shape while every gate stayed green.
  $tier = $facts['fleet_tier']
  unless $tier =~ String[1] {
    fail('fleet_tier external fact is missing or empty; check the facts.d mount')
  }

  # /var/tmp rather than /tmp: nothing applies catalogs in stage 1, but these
  # containers run Puppet as root and the file type follows symlinks, so a
  # world-writable sticky directory would be a privilege trap the first time
  # anything does apply one.
  file { '/var/tmp/fleet-tier':
    ensure  => file,
    content => "${tier}\n",
  }

  # Set by nothing except the compliance_markup enforcement backend, whose
  # check ships in this module's own SIMP/compliance_profiles/checks.yaml.
  #
  # The default names its own absence deliberately. If the backend stops being
  # consulted, or the modulepath glob stops finding this module's compliance
  # data, this parameter silently falls back -- and a check that only asserted
  # "the catalog compiled" would stay green while proving nothing.
  # gate-compliance.sh asserts the ENFORCED value, so that fallback turns it red.
  notify { 'fleet-compliance':
    message => $compliance_marker,
  }

  if $export_resources {
    $variant = $facts['fleet_export_variant'] ? {
      undef   => 'base',
      default => $facts['fleet_export_variant'],
    }

    @@notify { "fleet-exported-${facts['networking']['hostname']}-${variant}":
      message => "exported from ${facts['networking']['hostname']} (${variant})",
      tag     => 'fleet-export',
    }
  }

  if $collect_resources {
    Notify <<| tag == 'fleet-export' |>>
  }
}
