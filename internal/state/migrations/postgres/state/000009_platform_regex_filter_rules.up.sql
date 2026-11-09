-- Irreversible: ANY and MUST_NOT rules cannot be represented by the legacy
-- all-regexes-must-match format. Restore a pre-upgrade state to downgrade.
--
-- PostgreSQL port of the SQLite JSON1 version: json_each -> json_array_elements_text,
-- json_group_array -> json_agg, char(92) -> chr(92) (a literal backslash prefix
-- that marks MUST_NOT rules).
UPDATE platforms
SET regex_filters_json = (
	SELECT COALESCE(json_agg('*' || value), '[]'::json)::text
	FROM json_array_elements_text(platforms.regex_filters_json::json) AS value
)
WHERE json_array_length(regex_filters_json::json) > 1;

UPDATE platforms
SET regex_filters_json = (
	SELECT json_agg(chr(92) || value)::text
	FROM json_array_elements_text(platforms.regex_filters_json::json) AS value
)
WHERE json_array_length(regex_filters_json::json) = 1
	AND substr(regex_filters_json::json ->> 0, 1, 1) = '!';
