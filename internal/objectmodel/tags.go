package objectmodel

import (
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"
)

// S3 tag limits (see aws-sdk S3 docs: object tagging).
const (
	MaxTagCount       = 10
	MaxTagKeyLength   = 128
	MaxTagValueLength = 256
	reservedTagPrefix = "aws:"
)

// ParseTagHeader decodes an x-amz-tagging header value
// (URL-encoded key=value&key2=value2). Returns an error naming the
// violation (count, key length, value length, aws: prefix, bad encoding).
// An empty (or whitespace-only) header yields an empty tag set and no error.
func ParseTagHeader(v string) (map[string]string, error) {
	tags := make(map[string]string)
	if strings.TrimSpace(v) == "" {
		return tags, nil
	}
	for pair := range strings.SplitSeq(v, "&") {
		if pair == "" {
			continue
		}
		rawKey, rawVal, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, ErrInvalidArgument(fmt.Sprintf("the header 'x-amz-tagging' shall be encoded as URL query parameters, got malformed pair %q", pair))
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			return nil, ErrInvalidArgument(fmt.Sprintf("the tag key %q is not valid URL encoding: %v", rawKey, err))
		}
		val, err := url.QueryUnescape(rawVal)
		if err != nil {
			return nil, ErrInvalidArgument(fmt.Sprintf("the tag value %q is not valid URL encoding: %v", rawVal, err))
		}
		if _, dup := tags[key]; !dup {
			tags[key] = val // count every tag, even duplicate keys, against the limit check below
		}
	}
	if err := ValidateTags(tags); err != nil {
		return nil, err
	}
	return tags, nil
}

// ValidateTags applies the S3 limits to a tag map (also used for the
// ?tagging PUT XML body): at most MaxTagCount tags, keys at most
// MaxTagKeyLength bytes, values at most MaxTagValueLength bytes, no empty
// key, and the case-insensitive "aws:" prefix is reserved.
func ValidateTags(tags map[string]string) error {
	if len(tags) > MaxTagCount {
		return ErrInvalidArgument(fmt.Sprintf("object tags cannot be greater than %d", MaxTagCount))
	}
	for k, v := range tags {
		if len(k) == 0 {
			return ErrInvalidArgument("the tag key cannot be empty")
		}
		if len(k) > MaxTagKeyLength {
			return ErrInvalidArgument(fmt.Sprintf("the tag key %q is too long (max %d bytes)", k, MaxTagKeyLength))
		}
		if len(v) > MaxTagValueLength {
			return ErrInvalidArgument(fmt.Sprintf("the value for tag key %q is too long (max %d bytes)", k, MaxTagValueLength))
		}
		if strings.HasPrefix(strings.ToLower(k), reservedTagPrefix) {
			return ErrInvalidArgument(fmt.Sprintf("the tag key %q uses the reserved %q prefix", k, reservedTagPrefix))
		}
	}
	return nil
}

// Tagging is the GetObjectTagging/PutObjectTagging XML envelope.
type Tagging struct {
	XMLName xml.Name `xml:"Tagging"`
	TagSet  struct {
		Tags []Tag `xml:"Tag"`
	} `xml:"TagSet"`
}

// Tag is a single Key/Value pair inside a Tagging document.
type Tag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// TagsToXML renders the GetObjectTagging XML body. A nil or empty map
// renders an empty TagSet.
func TagsToXML(tags map[string]string) []byte {
	doc := Tagging{}
	for k, v := range tags {
		doc.TagSet.Tags = append(doc.TagSet.Tags, Tag{Key: k, Value: v})
	}
	out, err := xml.Marshal(doc)
	if err != nil {
		// xml.Marshal of this fixed shape cannot fail; fall back to an
		// empty TagSet rather than returning nil bytes.
		return []byte("<Tagging><TagSet></TagSet></Tagging>")
	}
	return out
}

// TagsFromXML parses the PutObjectTagging body (Tagging/TagSet/Tag with
// Key and Value elements) and validates the result against the S3 limits.
func TagsFromXML(body []byte) (map[string]string, error) {
	var doc Tagging
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, ErrInvalidArgument(fmt.Sprintf("failed to parse Tagging XML: %v", err))
	}
	tags := make(map[string]string, len(doc.TagSet.Tags))
	for _, t := range doc.TagSet.Tags {
		tags[t.Key] = t.Value
	}
	if err := ValidateTags(tags); err != nil {
		return nil, err
	}
	return tags, nil
}
