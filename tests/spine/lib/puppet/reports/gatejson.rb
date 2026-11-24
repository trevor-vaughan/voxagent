# The report processor every differential gate captures Ruby's report with:
#
#   puppet apply ... --libdir tests/spine/lib --reports gatejson --reportdir DIR
#
# writes DIR/gate.json, the report's `to_data_hash` as JSON, which the gates
# compare against voxagent's report. One file, run by the host's Puppet 8.10.0
# gem and by each image's openvox alike, so the capture cannot drift between
# gates.
#
# Why a processor and not a conversion of the stored YAML: `--reports store`
# writes `to_yaml` (Puppet 8.10.0 `reports/store.rb:38`), and the report's
# classes serialise through `to_data_hash` (`util/psych_support.rb:28-31`).
# `Event#to_data_hash`'s ToStringifiedConverter Base64-encodes any ASCII-8BIT
# String (`pops/serialization/to_stringified_converter.rb:97-103`) -- a content
# change's `desired_value`, every libselinux context -- so the stored file
# already holds a bare Base64 string with no type marker, and no later step
# can tell it from a value that really is that text. OpenVoxDB stores the
# plain string (measured; docs/maintainers/apply-spine.md, "What OpenVoxDB
# stores for a binary-encoded value"), and that is the contract compared. The
# puppetdb terminus is itself a report processor, so this one sees the live
# report exactly where the terminus does.
#
# Before `to_data_hash`, the strings the terminus sends are coerced to UTF-8
# exactly as the terminus coerces them; a coerced String is UTF-8, so the
# converter writes it plain. Nothing else about the hash changes. Scope is
# what the terminus passes through `coerce_payload` and nothing more.
# openvoxdb-termini 8.16.0, under puppet/ in openvox-agent 8.29.0's
# vendor_ruby: `reports/puppetdb.rb:36-79` (`report_to_hash`), `:109-123`
# (logs), `:128-138` (metrics), `:165-177` (events) and `:191-217` (resource
# statuses) build the payload, and `util/puppetdb/command.rb:20-57,76`
# coerces every String in it. Strings the terminus never sends (a status's
# `tags` or `provider_used`, an event's `historical_value`, a metric's label)
# are left as they are. The terminus drops unchanged resource statuses unless
# `include_unchanged_resources` is set; this coerces every status, which is
# the same thing for any status whose strings are already UTF-8.
#
# An event value that is not a String -- an Array, a Hash, a Symbol -- is left
# alone. The terminus sends it as `value.to_s`, a different shape rather than
# a different encoding; reproducing that is not this coercion's job.
require 'fileutils'
require 'json'

module VoxagentGate
  # Verbatim from openvoxdb-termini 8.16.0
  # `puppet/util/puppetdb/char_encoding.rb:13,66-82`, including the lossy
  # branch: a String with any invalid UTF-8 byte is re-tagged US-ASCII and
  # every non-ASCII byte becomes U+FFFD, so the gate compares PuppetDB's form.
  DEFAULT_INVALID_CHAR = "\ufffd"

  def self.coerce_to_utf8(str)
    str_copy = str.dup
    str_copy.force_encoding("UTF-8")
    if str_copy.valid_encoding?
      str_copy.encode!("UTF-8")
    else
      str_copy.force_encoding("US-ASCII")

      str_copy.encode!("UTF-8",
                       :invalid => :replace,
                       :undef => :replace,
                       :replace => DEFAULT_INVALID_CHAR)
    end
  end

  # coerce(obj, :@a, ...): coerce each named instance variable that holds a
  # String, or the String elements of one that holds an Array or a Set.
  def self.coerce(obj, *ivars)
    ivars.each do |ivar|
      v = obj.instance_variable_get(ivar)
      case v
      when String then obj.instance_variable_set(ivar, coerce_to_utf8(v))
      when Array, Set then v.map! { |e| e.is_a?(String) ? coerce_to_utf8(e) : e }
      end
    end
  end
end

Puppet::Reports.register_report(:gatejson) do
  desc "Write the report's to_data_hash as JSON to reportdir/gate.json, with the
  strings the puppetdb terminus sends coerced to UTF-8 as it coerces them.
  For voxagent's differential gates only."

  def process
    VoxagentGate.coerce(self, :@host, :@puppet_version, :@configuration_version,
                        :@environment, :@transaction_uuid, :@status, :@catalog_uuid,
                        :@code_id, :@job_id, :@cached_catalog_status)
    logs.each { |log| VoxagentGate.coerce(log, :@file, :@message, :@source, :@tags) }
    metrics.each_value do |metric|
      VoxagentGate.coerce(metric, :@name)
      metric.instance_variable_get(:@values).each do |value|
        value[0] = VoxagentGate.coerce_to_utf8(value[0]) if value[0].is_a?(String)
      end
    end
    resource_statuses.each_value do |status|
      next if status.nil?

      VoxagentGate.coerce(status, :@resource_type, :@title, :@file, :@containment_path)
      status.events.each do |event|
        VoxagentGate.coerce(event, :@status, :@property, :@desired_value,
                            :@previous_value, :@message)
      end
    end

    FileUtils.mkdir_p(Puppet[:reportdir])
    File.write(File.join(Puppet[:reportdir], 'gate.json'), to_data_hash.to_json + "\n")
  end
end
