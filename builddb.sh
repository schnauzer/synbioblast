#!/bin/bash

SYNBIOBLASTDIR="${SYNBIOBLASTDIR:-/var/synbioblast}"
echo "SynBioBLAST dir: $SYNBIOBLASTDIR"

BLASTDB="${BLASTDB:-$SYNBIOBLASTDIR/blastdbs}" 
echo "Storing blastdb files in $BLASTDB"

DBNAME="${DBNAME:-SynBioHub}"
echo "Using db name of $DBNAME"

TITLE="$DBNAME (generated $(date))"

find "$SYNBIOBLASTDIR/fastas" -mindepth 1 -name '*.fasta' -type f -exec cat {} \; | ./makeblastdb -dbtype nucl -title "$TITLE" -out "$BLASTDB/$DBNAME" -in -
