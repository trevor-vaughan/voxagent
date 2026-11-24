# M6 (06b): a comment changed, added and removed with ''; §8 item 3: h6's
# `# ` parses to comment '', so `comment => ''` is in sync.
host { 'h3': ip => '192.0.2.3', host_aliases => ['p'], comment => 'c3-new' }
host { 'h4': ip => '192.0.2.4', host_aliases => ['q'], comment => 'added' }
host { 'h5': ip => '192.0.2.5', host_aliases => ['r'], comment => '' }
host { 'h6': ip => '192.0.2.6', comment => '' }
