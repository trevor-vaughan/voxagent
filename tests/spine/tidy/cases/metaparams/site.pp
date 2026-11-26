# The Tidy's relationship metaparameters go to every File it generates.
notify { 'before': } -> tidy { '/srv/m': recurse => true, require => Notify['before'], notify => Notify['after'] } notify { 'after': }
