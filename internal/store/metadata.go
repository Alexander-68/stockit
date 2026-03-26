package store

import (
	"fmt"
	"slices"
	"strings"
)

type FieldKind string

const (
	KindText     FieldKind = "text"
	KindTextarea FieldKind = "textarea"
	KindDate     FieldKind = "date"
	KindInteger  FieldKind = "integer"
	KindReal     FieldKind = "real"
	KindEnum     FieldKind = "enum"
	KindStatus   FieldKind = "status"
	KindForeign  FieldKind = "foreign"
	KindPassword FieldKind = "password"
	KindBlob     FieldKind = "blob"
)

var (
	roleOptions     = []string{"admin", "user", "guest"}
	statusOptions   = []string{"Draft", "Under Review", "Active", "Inactive", "Hold", "Phase-Out", "Absolete"}
	poStatusOptions = []string{
		"issued",
		"approved",
		"sent",
		"confirmed",
		"paid",
		"prepared",
		"shipped",
		"delivered",
		"received",
		"complete",
		"inactive",
	}
	quoteStatusOptions      = []string{"active", "inactive"}
	salesOrderStatusOptions = []string{
		"confirmed",
		"preparing",
		"prepared",
		"shipped",
		"paid",
		"complete",
		"inactive",
	}
	currencyOptions = []string{"USD", "TWD", "CNY", "EUR"}
)

type Field struct {
	Column      string
	Label       string
	Kind        FieldKind
	Required    bool
	List        bool
	Editable    bool
	Sortable    bool
	Options     []string
	RefTable    string
	Accept      string
	Placeholder string
}

type SubtableDef struct {
	Table       string
	ForeignKey  string
	ParentLabel string
}

type TableDef struct {
	Name          string
	Label         string
	PrimaryKey    string
	TitleColumn   string
	ReferenceCols []string
	ParentTable   string
	ParentField   string
	ParentLabel   string
	Subtable      *SubtableDef
	ReadRoles     []string
	WriteRoles    []string
	Fields        []Field
	DefaultSort   string
	DefaultDesc   bool
	ImportEnabled bool
}

func AllTables() map[string]TableDef {
	tables := []TableDef{
		{
			Name:          "users",
			Label:         "Users",
			PrimaryKey:    "usr_id",
			TitleColumn:   "usr_login_name",
			ReferenceCols: []string{"usr_id", "usr_login_name", "usr_role"},
			ReadRoles:     []string{"admin"},
			WriteRoles:    []string{"admin"},
			DefaultSort:   "usr_login_name",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "usr_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "usr_login_name", Label: "login_name", Kind: KindText, Required: true, Editable: true, List: true, Sortable: true},
				{Column: "usr_password", Label: "password", Kind: KindPassword, Editable: true},
				{Column: "usr_role", Label: "role", Kind: KindEnum, Required: true, Editable: true, List: true, Sortable: true, Options: roleOptions},
				{Column: "usr_note", Label: "note", Kind: KindTextarea, Editable: true, List: true},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "customers",
			Label:         "Customers",
			PrimaryKey:    "cus_id",
			TitleColumn:   "cus_name_en",
			ReferenceCols: []string{"cus_id", "cus_name_en", "cus_phone"},
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "cus_name_en",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "cus_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "cus_name_en", Label: "name_en", Kind: KindText, Required: true, Editable: true, List: true, Sortable: true},
				{Column: "cus_name_zh", Label: "name_zh", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "cus_address_en", Label: "address_en", Kind: KindTextarea, Editable: true, List: true},
				{Column: "cus_address_zh", Label: "address_zh", Kind: KindTextarea, Editable: true},
				{Column: "cus_phone", Label: "phone", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "cus_ship_address_en", Label: "ship_address_en", Kind: KindTextarea, Editable: true},
				{Column: "cus_ship_address_zh", Label: "ship_address_zh", Kind: KindTextarea, Editable: true},
				{Column: "cus_contact_name", Label: "contact_name", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "cust_contact_email", Label: "contact_email", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "cus_note", Label: "note", Kind: KindTextarea, Editable: true, List: true},
				{Column: "usr_id", Label: "user_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "users"},
				{Column: "cus_status", Label: "status", Kind: KindStatus, Editable: true, List: true, Sortable: true, Options: statusOptions},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "suppliers",
			Label:         "Suppliers",
			PrimaryKey:    "sup_id",
			TitleColumn:   "sup_name_en",
			ReferenceCols: []string{"sup_id", "sup_code", "sup_name_en"},
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "sup_name_en",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "sup_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "sup_code", Label: "code", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "sup_name_en", Label: "name_en", Kind: KindText, Required: true, Editable: true, List: true, Sortable: true},
				{Column: "sup_name_zh", Label: "name_zh", Kind: KindText, Editable: true},
				{Column: "sup_type", Label: "type", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "sup_contact_name", Label: "contact_name", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "sup_contact_phone", Label: "contact_phone", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "sup_contact_email", Label: "contact_email", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "sup_contact_messanger", Label: "contact_messanger", Kind: KindText, Editable: true},
				{Column: "sup_fax", Label: "fax", Kind: KindText, Editable: true},
				{Column: "sup_address_en", Label: "address_en", Kind: KindTextarea, Editable: true},
				{Column: "sup_address_zh", Label: "address_zh", Kind: KindTextarea, Editable: true},
				{Column: "sup_factory_adress_zh", Label: "factory_adress_zh", Kind: KindTextarea, Editable: true},
				{Column: "sup_website", Label: "website", Kind: KindText, Editable: true},
				{Column: "sup_catalogue_url", Label: "catalogue_url", Kind: KindText, Editable: true},
				{Column: "sup_bank_name", Label: "bank_name", Kind: KindText, Editable: true},
				{Column: "sup_bank_account", Label: "bank_account", Kind: KindText, Editable: true},
				{Column: "sup_vat_number", Label: "vat_number", Kind: KindText, Editable: true},
				{Column: "sup_certificates", Label: "certificates", Kind: KindTextarea, Editable: true},
				{Column: "sup_note", Label: "note", Kind: KindTextarea, Editable: true},
				{Column: "usr_id", Label: "user_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "users"},
				{Column: "sup_status", Label: "status", Kind: KindStatus, Editable: true, List: true, Sortable: true, Options: statusOptions},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "locations",
			Label:         "Locations",
			PrimaryKey:    "loc_id",
			TitleColumn:   "loc_name",
			ReferenceCols: []string{"loc_id", "loc_name", "loc_zone"},
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "loc_name",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "loc_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "loc_name", Label: "name", Kind: KindText, Required: true, Editable: true, List: true, Sortable: true},
				{Column: "loc_address_en", Label: "address_en", Kind: KindTextarea, Editable: true, List: true},
				{Column: "loc_address_zh", Label: "address_zh", Kind: KindTextarea, Editable: true},
				{Column: "loc_zone", Label: "zone", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "loc_note", Label: "note", Kind: KindTextarea, Editable: true, List: true},
				{Column: "usr_id", Label: "user_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "users"},
				{Column: "loc_status", Label: "status", Kind: KindStatus, Editable: true, List: true, Sortable: true, Options: statusOptions},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "items",
			Label:         "Items",
			PrimaryKey:    "itm_id",
			TitleColumn:   "itm_sku",
			ReferenceCols: []string{"itm_id", "itm_sku", "itm_model"},
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "itm_sku",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "itm_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "itm_sku", Label: "sku", Kind: KindText, Required: true, Editable: true, List: true, Sortable: true},
				{Column: "itm_model", Label: "model", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "itm_description", Label: "description", Kind: KindTextarea, Editable: true, List: true},
				{Column: "itm_value", Label: "value", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "itm_type", Label: "type", Kind: KindEnum, Editable: true, List: true, Sortable: true, Options: []string{"final", "part", "assembly"}},
				{Column: "itm_pic", Label: "picture", Kind: KindBlob, Editable: true, List: true, Accept: "image/*"},
				{Column: "itm_measure_unit", Label: "measure_unit", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "itm_note", Label: "note", Kind: KindTextarea, Editable: true, List: true},
				{Column: "usr_id", Label: "user_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "users"},
				{Column: "itm_status", Label: "status", Kind: KindStatus, Editable: true, List: true, Sortable: true, Options: statusOptions},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "boms",
			Label:         "BOM",
			PrimaryKey:    "bom_id",
			TitleColumn:   "bom_doc_number",
			ReferenceCols: []string{"bom_id", "bom_doc_number", "bom_status"},
			Subtable: &SubtableDef{
				Table:       "bom_components",
				ForeignKey:  "bom_id",
				ParentLabel: "Selected BOM",
			},
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "bom_doc_number",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "bom_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "bom_doc_number", Label: "doc_number", Kind: KindText, Required: true, Editable: true, List: true, Sortable: true},
				{Column: "bom_doc_date", Label: "doc_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "itm_id", Label: "item_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "items"},
				{Column: "bom_note", Label: "note", Kind: KindTextarea, Editable: true, List: true},
				{Column: "usr_id", Label: "user_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "users"},
				{Column: "bom_status", Label: "status", Kind: KindStatus, Editable: true, List: true, Sortable: true, Options: statusOptions},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "bom_components",
			Label:         "BOM Components",
			PrimaryKey:    "boc_id",
			TitleColumn:   "boc_id",
			ReferenceCols: []string{"boc_id", "bom_id", "itm_id"},
			ParentTable:   "boms",
			ParentField:   "bom_id",
			ParentLabel:   "Selected BOM",
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "boc_id",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "boc_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "bom_id", Label: "bom_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "boms"},
				{Column: "itm_id", Label: "item_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "items"},
				{Column: "boc_qty", Label: "qty", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "boc_note", Label: "note", Kind: KindTextarea, Editable: true, List: true},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "purchase_orders",
			Label:         "Purchase Order",
			PrimaryKey:    "por_id",
			TitleColumn:   "por_doc_number",
			ReferenceCols: []string{"por_id", "por_doc_number", "por_status"},
			Subtable: &SubtableDef{
				Table:       "po_components",
				ForeignKey:  "por_id",
				ParentLabel: "Selected Purchase Order",
			},
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "por_doc_number",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "por_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "sup_id", Label: "supplier_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "suppliers"},
				{Column: "por_doc_number", Label: "doc_number", Kind: KindText, Required: true, Editable: true, List: true, Sortable: true},
				{Column: "por_doc_date", Label: "doc_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "itm_id", Label: "item_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "items"},
				{Column: "por_ship_date", Label: "ship_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "por_paid_date", Label: "paid_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "usr_id", Label: "user_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "users"},
				{Column: "por_status", Label: "status", Kind: KindEnum, Editable: true, List: true, Sortable: true, Options: poStatusOptions},
				{Column: "por_note", Label: "note", Kind: KindTextarea, Editable: true, List: true},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "quotes",
			Label:         "Quote",
			PrimaryKey:    "qot_id",
			TitleColumn:   "qot_doc_number",
			ReferenceCols: []string{"qot_id", "qot_doc_number", "qot_status"},
			Subtable: &SubtableDef{
				Table:       "quote_components",
				ForeignKey:  "qot_id",
				ParentLabel: "Selected Quote",
			},
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "qot_doc_number",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "qot_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "sup_id", Label: "supplier_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "suppliers"},
				{Column: "qot_doc_number", Label: "doc_number", Kind: KindText, Required: true, Editable: true, List: true, Sortable: true},
				{Column: "qot_doc_date", Label: "doc_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "itm_id", Label: "item_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "items"},
				{Column: "usr_id", Label: "user_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "users"},
				{Column: "qot_status", Label: "status", Kind: KindEnum, Editable: true, List: true, Sortable: true, Options: quoteStatusOptions},
				{Column: "qot_note", Label: "note", Kind: KindTextarea, Editable: true, List: true},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "quote_components",
			Label:         "Quote Components",
			PrimaryKey:    "qoc_id",
			TitleColumn:   "qoc_id",
			ReferenceCols: []string{"qoc_id", "qot_id", "itm_id"},
			ParentTable:   "quotes",
			ParentField:   "qot_id",
			ParentLabel:   "Selected Quote",
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "qoc_id",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "qoc_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "qot_id", Label: "qot_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "quotes"},
				{Column: "itm_id", Label: "item_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "items"},
				{Column: "qot_moq", Label: "moq", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "qot_qty", Label: "qty", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "qot_price", Label: "price", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "qot_currency", Label: "currency", Kind: KindEnum, Editable: true, List: true, Sortable: true, Options: currencyOptions},
				{Column: "qot_lead_time", Label: "lead_time", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "po_components",
			Label:         "PO Components",
			PrimaryKey:    "poc_id",
			TitleColumn:   "poc_id",
			ReferenceCols: []string{"poc_id", "por_id", "itm_id"},
			ParentTable:   "purchase_orders",
			ParentField:   "por_id",
			ParentLabel:   "Selected Purchase Order",
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "poc_id",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "poc_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "por_id", Label: "por_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "purchase_orders"},
				{Column: "itm_id", Label: "item_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "items"},
				{Column: "poc_qty", Label: "qty", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "poc_price", Label: "price", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "poc_currency", Label: "currency", Kind: KindEnum, Editable: true, List: true, Sortable: true, Options: currencyOptions},
				{Column: "poc_shipped_date", Label: "shipped_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "poc_delivered_date", Label: "delivered_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "poc_delivered_qty", Label: "delivered_qty", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "poc_received_date", Label: "received_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "poc_received_qty", Label: "received_qty", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "sales_orders",
			Label:         "Sales Order",
			PrimaryKey:    "sor_id",
			TitleColumn:   "sor_doc_number",
			ReferenceCols: []string{"sor_id", "sor_doc_number", "sor_status"},
			Subtable: &SubtableDef{
				Table:       "sales_order_components",
				ForeignKey:  "sor_id",
				ParentLabel: "Selected Sales Order",
			},
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "sor_doc_number",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "sor_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "cus_id", Label: "customer_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "customers"},
				{Column: "sor_doc_number", Label: "doc_number", Kind: KindText, Required: true, Editable: true, List: true, Sortable: true},
				{Column: "sor_doc_date", Label: "doc_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "sor_ship_date", Label: "ship_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "sor_paid_date", Label: "paid_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "usr_id", Label: "user_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "users"},
				{Column: "sor_status", Label: "status", Kind: KindEnum, Editable: true, List: true, Sortable: true, Options: salesOrderStatusOptions},
				{Column: "sor_note", Label: "note", Kind: KindTextarea, Editable: true, List: true},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
		{
			Name:          "sales_order_components",
			Label:         "Sales Order Components",
			PrimaryKey:    "soc_id",
			TitleColumn:   "soc_id",
			ReferenceCols: []string{"soc_id", "sor_id", "itm_id"},
			ParentTable:   "sales_orders",
			ParentField:   "sor_id",
			ParentLabel:   "Selected Sales Order",
			ReadRoles:     []string{"admin", "user", "guest"},
			WriteRoles:    []string{"admin", "user"},
			DefaultSort:   "soc_id",
			ImportEnabled: true,
			Fields: []Field{
				{Column: "soc_id", Label: "id", Kind: KindInteger, List: true, Sortable: true},
				{Column: "sor_id", Label: "sor_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "sales_orders"},
				{Column: "itm_id", Label: "item_id", Kind: KindForeign, Editable: true, List: true, Sortable: true, RefTable: "items"},
				{Column: "sor_qty", Label: "qty", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "sor_price", Label: "price", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "sor_currency", Label: "currency", Kind: KindEnum, Editable: true, List: true, Sortable: true, Options: currencyOptions},
				{Column: "sor_ship_date", Label: "ship_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "sor_shipped_date", Label: "shipped_date", Kind: KindDate, Editable: true, List: true, Sortable: true},
				{Column: "sor_shipped_qty", Label: "shipped_qty", Kind: KindReal, Editable: true, List: true, Sortable: true},
				{Column: "sor_shipped_trackno", Label: "shipped_trackno", Kind: KindText, Editable: true, List: true, Sortable: true},
				{Column: "soc_note", Label: "note", Kind: KindTextarea, Editable: true, List: true},
				{Column: "created_at", Label: "created_at", Kind: KindText, List: true, Sortable: true},
			},
		},
	}

	lookup := make(map[string]TableDef, len(tables))
	for _, table := range tables {
		lookup[table.Name] = table
	}
	return lookup
}

func TablesForRole(role string) []TableDef {
	all := AllTables()
	names := make([]string, 0, len(all))
	for name, table := range all {
		if table.CanRead(role) {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	result := make([]TableDef, 0, len(names))
	for _, name := range names {
		table := all[name]
		if table.IsSubtable() {
			continue
		}
		result = append(result, table)
	}
	return result
}

func (t TableDef) Field(column string) (Field, bool) {
	for _, field := range t.Fields {
		if field.Column == column {
			return field, true
		}
	}
	return Field{}, false
}

func (t TableDef) ListFields() []Field {
	return filterFields(t.Fields, func(field Field) bool { return field.List })
}

func (t TableDef) EditableFields() []Field {
	return filterFields(t.Fields, func(field Field) bool { return field.Editable })
}

func (t TableDef) Columns() []string {
	columns := make([]string, 0, len(t.Fields))
	for _, field := range t.Fields {
		columns = append(columns, field.Column)
	}
	return columns
}

func (t TableDef) InsertableColumns(values map[string]any) []string {
	columns := make([]string, 0, len(values))
	for _, field := range t.EditableFields() {
		if _, ok := values[field.Column]; ok {
			columns = append(columns, field.Column)
		}
	}
	return columns
}

func (t TableDef) UpdatableColumns(values map[string]any) []string {
	return t.InsertableColumns(values)
}

func (t TableDef) CanRead(role string) bool {
	return slices.Contains(t.ReadRoles, role)
}

func (t TableDef) CanWrite(role string) bool {
	return slices.Contains(t.WriteRoles, role)
}

func (t TableDef) IsSubtable() bool {
	return strings.TrimSpace(t.ParentTable) != "" && strings.TrimSpace(t.ParentField) != ""
}

func (t TableDef) ReferenceColumns() []string {
	if len(t.ReferenceCols) > 0 {
		return slices.Clone(t.ReferenceCols)
	}

	columns := []string{t.PrimaryKey}
	if t.TitleColumn != "" && t.TitleColumn != t.PrimaryKey {
		columns = append(columns, t.TitleColumn)
	}
	return columns
}

func (t TableDef) SortColumn(requested string) string {
	if requested == "" {
		return t.DefaultSort
	}
	field, ok := t.Field(requested)
	if !ok || !field.Sortable {
		return t.DefaultSort
	}
	return field.Column
}

func (t TableDef) DisplayValue(record map[string]any) string {
	value := record[t.TitleColumn]
	switch typed := value.(type) {
	case string:
		if typed != "" {
			return typed
		}
	case []byte:
		if len(typed) > 0 {
			return fmt.Sprintf("%d bytes", len(typed))
		}
	}

	if primary, ok := record[t.PrimaryKey]; ok {
		return fmt.Sprint(primary)
	}
	return ""
}

func DisplayValue(field Field, value any) string {
	if value == nil {
		return ""
	}

	switch typed := value.(type) {
	case []byte:
		if len(typed) == 0 {
			return ""
		}
		return fmt.Sprintf("%d bytes", len(typed))
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func NormalizeCSVHeader(header string) string {
	return strings.ToLower(strings.TrimSpace(strings.ReplaceAll(header, " ", "_")))
}

func filterFields(fields []Field, allow func(Field) bool) []Field {
	filtered := make([]Field, 0, len(fields))
	for _, field := range fields {
		if allow(field) {
			filtered = append(filtered, field)
		}
	}
	return filtered
}
