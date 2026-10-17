package lower

import (
	"maps"
	"slices"

	check "github.com/d7z-team/mini-go/compiler/semantic"
	"github.com/d7z-team/mini-go/compiler/types"
)

func hirTypeString(table *types.TypeTable, ref types.TypeRef) string {
	return types.FormatWithTable(table, ref)
}

func registerTestTypeDecls(l *lowerer) {
	for _, name := range slices.Sorted(maps.Keys(l.typeDecls)) {
		l.registerDependencyTypeCatalog(name, moduleExportInfo{
			ModulePath: l.modulePath, Kind: check.ObjectType,
			Underlying: l.resolveSourceType(l.typeDecls[name]),
		})
	}
}
