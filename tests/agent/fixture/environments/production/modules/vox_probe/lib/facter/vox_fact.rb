# Custom fact delivered by pluginsync; the manifest reads it, so a run that
# skipped pluginsync (or did not use the fact) cannot produce the notify text.
Facter.add(:vox_fact) do
  setcode { 'from-plugin' }
end
