package serverruntime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestInfrastructureStartupVerifiesSchemaWithoutApplyingMigrations(test *testing.T) {
	source, err := os.ReadFile("infrastructure.go")
	if err != nil {
		test.Fatal(err)
	}
	positions := token.NewFileSet()
	parsed, err := parser.ParseFile(positions, "infrastructure.go", source, 0)
	if err != nil {
		test.Fatal(err)
	}
	var startup *ast.FuncDecl
	ast.Inspect(parsed, func(node ast.Node) bool {
		if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "ApplyMigrations" {
			test.Errorf("infrastructure must not apply migrations: %s", positions.Position(selector.Pos()))
		}
		if function, ok := node.(*ast.FuncDecl); ok && function.Name.Name == "OpenInfrastructure" {
			startup = function
		}
		return true
	})
	if startup == nil || startup.Body == nil {
		test.Fatal("actual infrastructure startup is missing")
	}
	body := string(source[positions.Position(startup.Body.Pos()).Offset:positions.Position(startup.Body.End()).Offset])
	previous := -1
	for _, required := range []string{
		"if config.RequirePostgres || dsn != \"\" {",
		"app.ValidateProductionStore(storage)",
		"directory := config.MigrationsDirectory",
		"directory = \"schemas/db/migrations\"",
		"err = storage.VerifyMigrations(ctx, directory)",
		"return nil, fmt.Errorf(\"verify schema (run cxt-admin migrate --apply before serving): %w\", err)",
		"log.Print(\"schema: verified\")",
		"infrastructure.DocumentProfile = documentquery.VerifiedRoots",
		"publication.ConfigureLegacyByteRangePublication(config.LegacyByteRangePublication)",
		"derivedPublication.ConfigureLegacyDerivedByteRangePublication(config.LegacyDerivedBytePublication)",
		"config.execution.configure(ctx, storage)",
	} {
		position := strings.Index(body, required)
		if position < 0 || position <= previous {
			test.Fatalf("startup schema verification or configuration order changed at %q", required)
		}
		previous = position
	}
	for _, forbidden := range []string{"os.Getenv", "os.LookupEnv", "os.Environ", "migrations: applied"} {
		if strings.Contains(string(source), forbidden) {
			test.Errorf("infrastructure exposes runtime migration policy or a DDL success claim: %q", forbidden)
		}
	}
}

func TestInfrastructureConfigHasNoMigrationWritePolicy(test *testing.T) {
	configuration := reflect.TypeOf(InfrastructureConfig{})
	expected := []string{
		"DSN",
		"DataDir",
		"RequirePostgres",
		"MigrationsDirectory",
		"LegacyByteRangePublication",
		"LegacyDerivedBytePublication",
		"execution",
	}
	if configuration.NumField() != len(expected) {
		test.Fatal("infrastructure configuration must not expose a migration write switch or callback")
	}
	for position, name := range expected {
		field := configuration.Field(position)
		if field.Name != name || field.Type.Kind() == reflect.Func || field.Type.Kind() == reflect.Interface {
			test.Fatalf("infrastructure configuration introduced arbitrary startup policy: %s", field.Name)
		}
	}
}
