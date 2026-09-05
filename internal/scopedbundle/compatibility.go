package scopedbundle

import (
	"fmt"
	"strings"
)

// CheckCompatibility proves that writing bundle rows to target cannot silently
// discard a non-default source value or require an unavailable target value.
// It is deliberately narrower than a schema migration: no DDL is emitted and
// incompatible schemas are only reported.
func CheckCompatibility(bundle Bundle, target Schema) error {
	if err := bundle.validateShape(); err != nil {
		return err
	}
	if target.Version < 53 {
		return fmt.Errorf("target schema v%d is below minimum v53", target.Version)
	}
	for _, sourceTable := range bundle.Tables {
		targetColumns, ok := target.Tables[sourceTable.Name]
		if !ok {
			return fmt.Errorf("target required table %s is missing", sourceTable.Name)
		}
		targetByName := columnsByName(targetColumns)
		sourceByName := columnsByName(sourceTable.Columns)

		for sourceIndex, sourceColumn := range sourceTable.Columns {
			targetColumn, exists := targetByName[sourceColumn.Name]
			if !exists {
				if columnHasNonDefaultValue(sourceTable, sourceIndex, sourceColumn) {
					return fmt.Errorf("source-only column %s.%s has a non-default scoped value", sourceTable.Name, sourceColumn.Name)
				}
				continue
			}
			if typeFamily(sourceColumn.SQLType) != typeFamily(targetColumn.SQLType) {
				return fmt.Errorf("incompatible column %s.%s: source %s target %s", sourceTable.Name, sourceColumn.Name, sourceColumn.SQLType, targetColumn.SQLType)
			}
		}

		for _, targetColumn := range targetColumns {
			if targetColumn.Generated {
				continue
			}
			if _, exists := sourceByName[targetColumn.Name]; exists {
				continue
			}
			if !targetColumn.Nullable && targetColumn.Default == nil {
				return fmt.Errorf("target-only column %s.%s is required and has no default", sourceTable.Name, targetColumn.Name)
			}
		}
	}
	return nil
}

func columnsByName(columns []Column) map[string]Column {
	result := make(map[string]Column, len(columns))
	for _, column := range columns {
		result[column.Name] = column
	}
	return result
}

func columnHasNonDefaultValue(table Table, index int, column Column) bool {
	for _, row := range table.Rows {
		cell := row.Cells[index]
		if cell.Null {
			continue
		}
		if column.Default == nil || cell.Text != *column.Default {
			return true
		}
	}
	return false
}

func typeFamily(sqlType string) string {
	typeName := strings.ToLower(strings.TrimSpace(sqlType))
	if index := strings.IndexByte(typeName, '('); index >= 0 {
		typeName = typeName[:index]
	}
	switch typeName {
	case "char", "varchar", "tinytext", "text", "mediumtext", "longtext", "enum", "set", "json":
		return "text"
	case "bit", "bool", "boolean", "tinyint", "smallint", "mediumint", "int", "integer", "bigint", "decimal", "numeric", "float", "double", "real":
		return "number"
	case "date", "datetime", "timestamp", "time", "year":
		return "time"
	case "binary", "varbinary", "tinyblob", "blob", "mediumblob", "longblob":
		return "binary"
	default:
		return typeName
	}
}
