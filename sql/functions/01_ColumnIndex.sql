CREATE OR REPLACE FUNCTION ColumnIndex AS (Database, Table, Column) ->
    (SELECT position FROM system.columns WHERE (database = Database) AND (table = Table) AND (name = Column));
