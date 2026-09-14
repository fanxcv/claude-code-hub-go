package jobs

// 本文件把 vendor 展示表的两项查询导出给管理面（/api/prices/vendors 的降级分支需要它们）。
//
// 只做转发，不在管理面另抄一份表：图标映射表是 src/lib/model-vendor/vendor-icon-map.json 的
// 内嵌逐字节副本，显示名表同理（见 vendors.go 文件头）。抄第二份就等于多一处会腐烂的副本。

// VendorDisplayName 返回 vendor slug 的展示名；表里没有时回落到 slug 本身。
func VendorDisplayName(slug string) string {
	return vendorDisplayName(slug)
}

// VendorIconFileForVendor 返回 vendor 的图标文件条目；没有对应图标时 ok 为 false。
func VendorIconFileForVendor(slug string) (VendorIconFileEntry, bool) {
	return iconFileForVendor(slug)
}
